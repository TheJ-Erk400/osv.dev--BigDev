package main

import (
	"encoding/json"
	"flag"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/google/osv/vulnfeeds/cves"
	"github.com/google/osv/vulnfeeds/utility"
	"github.com/google/osv/vulnfeeds/vulns"
)

const (
	defaultCvePath        = "cve_jsons"
	defaultPartsInputPath = "parts"
	defaultOSVOutputPath  = "osv_output"
	defaultCVEListPath    = "."

	alpineEcosystem          = "Alpine"
	alpineSecurityTrackerURL = "https://security.alpinelinux.org/vuln"
	debianEcosystem          = "Debian"
	debianSecurityTrackerURL = "https://security-tracker.debian.org/tracker"
)

var Logger utility.LoggerWrapper

func main() {
	var logCleanup func()
	Logger, logCleanup = utility.CreateLoggerWrapper("combine-to-osv")
	defer logCleanup()

	cvePath := flag.String("cvePath", defaultCvePath, "Path to CVE file")
	partsInputPath := flag.String("partsPath", defaultPartsInputPath, "Path to CVE file")
	osvOutputPath := flag.String("osvOutputPath", defaultOSVOutputPath, "Path to CVE file")
	cveListPath := flag.String("cveListPath", defaultCVEListPath, "Path to clone of https://github.com/CVEProject/cvelistV5")
	flag.Parse()

	err := os.MkdirAll(*cvePath, 0755)
	if err != nil {
		Logger.Fatalf("Can't create output path: %s", err)
	}
	err = os.MkdirAll(*osvOutputPath, 0755)
	if err != nil {
		Logger.Fatalf("Can't create output path: %s", err)
	}

	allCves := loadAllCVEs(*cvePath)
	allParts, cveModifiedMap := loadParts(*partsInputPath)
	combinedData := combineIntoOSV(allCves, allParts, *cveListPath, cveModifiedMap)
	writeOSVFile(combinedData, *osvOutputPath)
}

// getModifiedTime gets the modification time of a given file
// This function assumes that the modified time on disk matches with it in GCS
func getModifiedTime(filePath string) (time.Time, error) {
	var emptyTime time.Time
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return emptyTime, err
	}
	parsedTime := fileInfo.ModTime()

	return parsedTime, err
}

// loadInnerParts loads second level folder for the loadParts function
//
// Parameters:
//   - innerPartInputPath: The inner part path, such as "parts/alpine"
//   - output: A map to store all PackageInfos for each CVE ID
//   - cvePartsModifiedTime: A map tracking the latest modification time of each CVE part files
func loadInnerParts(innerPartInputPath string, output map[cves.CVEID][]vulns.PackageInfo, cvePartsModifiedTime map[cves.CVEID]time.Time) {
	dirInner, err := os.ReadDir(innerPartInputPath)
	if err != nil {
		Logger.Fatalf("Failed to read dir %q: %s", innerPartInputPath, err)
	}
	for _, entryInner := range dirInner {
		if !strings.HasSuffix(entryInner.Name(), ".json") {
			continue
		}
		filePath := path.Join(innerPartInputPath, entryInner.Name())
		file, err := os.Open(filePath)
		if err != nil {
			Logger.Fatalf("Failed to open PackageInfo JSON %q: %s", path.Join(innerPartInputPath, entryInner.Name()), err)
		}
		defer file.Close()
		var pkgInfos []vulns.PackageInfo
		err = json.NewDecoder(file).Decode(&pkgInfos)
		if err != nil {
			Logger.Fatalf("Failed to decode %q: %s", file.Name(), err)
		}

		// Turns CVE-2022-12345.alpine.json into CVE-2022-12345
		cveId := cves.CVEID(strings.Split(entryInner.Name(), ".")[0])
		output[cveId] = append(output[cveId], pkgInfos...)

		Logger.Infof(
			"Loaded Item: %s", entryInner.Name())

		// Updates the latest OSV parts modified time of each CVE
		modifiedTime, err := getModifiedTime(filePath)
		if err != nil {
			Logger.Warnf("Failed to get modified time of %s: %s", filePath, err)
			continue
		}
		existingDate, exists := cvePartsModifiedTime[cveId]
		if !exists || modifiedTime.After(existingDate) {
			cvePartsModifiedTime[cveId] = modifiedTime
		}
	}
}

// loadParts loads files generated by other executables in the cmd folder.
//
// Expects directory structure of:
//
// - <partsInputPath>/
//   - alpineParts/
//   - CVE-2020-1234.alpine.json
//   - ...
//   - debianParts/
//   - ...
//
// ## Returns
// A mapping of "CVE-ID": []<Affected Package Information>
// A mapping of "CVE-ID": time.Time (the latest modified time of its part files)
func loadParts(partsInputPath string) (map[cves.CVEID][]vulns.PackageInfo, map[cves.CVEID]time.Time) {
	dir, err := os.ReadDir(partsInputPath)
	if err != nil {
		Logger.Fatalf("Failed to read dir %q: %s", partsInputPath, err)
	}
	output := map[cves.CVEID][]vulns.PackageInfo{}
	cvePartsModifiedTime := make(map[cves.CVEID]time.Time)
	for _, entry := range dir {
		if !entry.IsDir() {
			Logger.Warnf("Unexpected file entry %q in %s", entry.Name(), partsInputPath)
			continue
		}
		// map is already a reference type, so no need to pass in a pointer
		loadInnerParts(path.Join(partsInputPath, entry.Name()), output, cvePartsModifiedTime)
	}
	return output, cvePartsModifiedTime
}

// combineIntoOSV creates OSV entry by combining loaded CVEs from NVD and PackageInfo information from security advisories.
func combineIntoOSV(loadedCves map[cves.CVEID]cves.Vulnerability, allParts map[cves.CVEID][]vulns.PackageInfo, cveList string, cvePartsModifiedTime map[cves.CVEID]time.Time) map[cves.CVEID]*vulns.Vulnerability {
	Logger.Infof("Begin writing OSV files from %d parts", len(allParts))
	convertedCves := map[cves.CVEID]*vulns.Vulnerability{}
	for cveId, cve := range loadedCves {
		if len(allParts[cveId]) == 0 {
			continue
		}
		convertedCve, _ := vulns.FromCVE(cveId, cve.CVE)
		if len(cveList) > 0 {
			// Best-effort attempt to mark a disputed CVE as withdrawn.
			modified, err := vulns.CVEIsDisputed(convertedCve, cveList)
			if err != nil {
				Logger.Warnf("Unable to determine CVE dispute status of %s: %v", convertedCve.ID, err)
			}
			if err == nil && modified != "" {
				convertedCve.Withdrawn = modified
			}
		}

		addedDebianURL := false
		addedAlpineURL := false
		for _, pkgInfo := range allParts[cveId] {
			convertedCve.AddPkgInfo(pkgInfo)
			if strings.HasPrefix(pkgInfo.Ecosystem, debianEcosystem) && !addedDebianURL {
				addReference(string(cveId), debianEcosystem, convertedCve)
				addedDebianURL = true
			} else if strings.HasPrefix(pkgInfo.Ecosystem, alpineEcosystem) && !addedAlpineURL {
				addReference(string(cveId), alpineEcosystem, convertedCve)
				addedAlpineURL = true
			}
		}

		cveModified, _ := time.Parse(time.RFC3339, convertedCve.Modified)
		if cvePartsModifiedTime[cveId].After(cveModified) {
			convertedCve.Modified = cvePartsModifiedTime[cveId].Format(time.RFC3339)
		}
		convertedCves[cveId] = convertedCve
	}
	Logger.Infof("Ended writing %d OSV files", len(convertedCves))
	return convertedCves
}

// writeOSVFile writes out the given osv objects into individual json files
func writeOSVFile(osvData map[cves.CVEID]*vulns.Vulnerability, osvOutputPath string) {
	for vId, osv := range osvData {
		file, err := os.OpenFile(path.Join(osvOutputPath, string(vId)+".json"), os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			Logger.Fatalf("Failed to create/open file to write: %s", err)
		}
		encoder := json.NewEncoder(file)
		encoder.SetIndent("", "  ")
		err = encoder.Encode(osv)
		if err != nil {
			Logger.Fatalf("Failed to encode OSVs")
		}
		file.Close()
	}

	Logger.Infof("Successfully written %d OSV files", len(osvData))
}

// loadAllCVEs loads the downloaded CVE's from the NVD database into memory.
func loadAllCVEs(cvePath string) map[cves.CVEID]cves.Vulnerability {
	dir, err := os.ReadDir(cvePath)
	if err != nil {
		Logger.Fatalf("Failed to read dir %s: %s", cvePath, err)
	}

	result := make(map[cves.CVEID]cves.Vulnerability)

	for _, entry := range dir {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		file, err := os.Open(path.Join(cvePath, entry.Name()))
		if err != nil {
			Logger.Fatalf("Failed to open CVE JSON %q: %s", path.Join(cvePath, entry.Name()), err)
		}
		var nvdcve cves.CVEAPIJSON20Schema
		err = json.NewDecoder(file).Decode(&nvdcve)
		if err != nil {
			Logger.Fatalf("Failed to decode JSON in %q: %s", file.Name(), err)
		}

		for _, item := range nvdcve.Vulnerabilities {
			result[item.CVE.ID] = item
		}
		Logger.Infof("Loaded CVE: %s", entry.Name())
		file.Close()
	}
	return result
}

// addReference adds the related security tracker URL to a given vulnerability's references
func addReference(cveId string, ecosystem string, convertedCve *vulns.Vulnerability) {
	securityReference := vulns.Reference{Type: "ADVISORY"}
	if ecosystem == alpineEcosystem {
		securityReference.URL, _ = url.JoinPath(alpineSecurityTrackerURL, cveId)
	} else if ecosystem == debianEcosystem {
		securityReference.URL, _ = url.JoinPath(debianSecurityTrackerURL, cveId)
	}

	if securityReference.URL == "" {
		return
	}

	convertedCve.References = append(convertedCve.References, securityReference)
}
