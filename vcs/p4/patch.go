package p4

import (
	"fmt"
	"github.com/juju/errors"
	"github.com/pmezard/go-difflib/difflib"
	"io/ioutil"
	"regexp"
	"strings"
)

// Todo(Junyu) add patch function for added and removed files

// Patch returns a diff of any uncommited changes (stagged and unstaged).
func (r *Repo) Patches() ([]string, error) {
	var patches []string
	diffPatch, err := stagedAndUnstagedPatch()
	if err != nil {
		return nil, errors.Trace(err)
	}
	// Don't add a patch for empty diffs
	if !strings.Contains(diffPatch, "File(s) not opened") && diffPatch!="" {
		patches = append(patches, diffPatch)
	}

	delFiles, err := deletedFiles()
	if err != nil {
		return nil, errors.Trace(err)
	}
	for _, file := range delFiles {
		patches = append(patches, file)
	}

	files, err := newFiles()
	if err != nil {
		return nil, errors.Trace(err)
	}
	for _, file := range files {
		patches = append(patches, file)
	}
	return patches, nil
}

func stagedAndUnstagedPatch() (string, error) {
	var relativeFilePath = ""
	_, err := p4CMD("reconcile", "-e")
	if err != nil {
		return "", errors.Trace(err)
	}

	diff, err := p4CMD("diff", "-du")
	if err != nil {
		return "", errors.Trace(err)
	}
	out, err := p4CMD("-Ztag", "-F", "%action% %depotFile%", "status")
	if err != nil {
		return "", errors.Trace(err)
	}

	reg := regexp.MustCompile("(?m)^edit .+")
	filePaths := reg.FindAllString(out, -1)
	if len(filePaths) == 0 {
		return "", nil
	}

	for _, filePath := range filePaths {
		relativeFilePath = ""
		filePath = strings.Split(filePath, "edit ")[1]
		out, err := p4CMD("where", filePath)
		if err != nil {
			return "", errors.Trace(err)
		}
		localPath := strings.Split(strings.TrimSpace(out), " ")[2]
		depotFile, err := p4CMD("-Ztag", "-F", "%depotFile%", "where", filePath)
		if err != nil {
			return "", errors.Trace(err)
		}
		pathElements := strings.Split(strings.Split(strings.TrimSpace(depotFile), "...")[0], "/")
		for i := 4; i < len(pathElements)-1; i++ {
			relativeFilePath += pathElements[i] + "/"
		}
		relativeFilePath += pathElements[len(pathElements)-1]
		diff = strings.Replace(diff, strings.Split(filePath, "...")[0], relativeFilePath, 1)
		diff = strings.Replace(diff, strings.Split(localPath, "...")[0], relativeFilePath, 1)
	}
	return diff, nil
}

// deletedFile creates a slice of custom patch strings about the deleted files
// ie. delete hello.cpp
func deletedFiles() ([]string, error) {
	var relativeFilePath = ""
	out, err := p4CMD("-Ztag", "-F", "%action% %depotFile%", "status")
	if err != nil {
		return nil, errors.Trace(err)
	}
	reg := regexp.MustCompile("(?m)^.*delete .+")
	matches := reg.FindAllString(out, -1)
	if len(matches) == 0 {
		return nil, nil
	}

	for k, match := range matches {
		filePath := strings.Split(match, " ")[1]
		depotFile, err := p4CMD("-Ztag", "-F", "%depotFile%", "where", filePath)
		if err != nil {
			return nil, errors.Trace(err)
		}
		pathElements := strings.Split(strings.Split(strings.TrimSpace(depotFile), "...")[0], "/")
		for i := 4; i < len(pathElements)-1; i++ {
			relativeFilePath += pathElements[i] + "/"
		}
		relativeFilePath += pathElements[len(pathElements)-1]
		matches[k] = strings.Replace(match, match, fmt.Sprintf("delete %s",relativeFilePath), 1)
		relativeFilePath=""
	}
	return matches, nil
}

func newFiles() ([]string, error) {
	var patches []string
	var relativeFilePath = ""
	out, err := p4CMD("-Ztag", "-F", "%action% %depotFile%", "status")
	if err != nil {
		return nil, errors.Trace(err)
	}

	reg := regexp.MustCompile("(?m)^.*add .+")
	filePaths := reg.FindAllString(out, -1)
	if len(filePaths) == 0 {
		return nil, nil
	}

	for _, filePath := range filePaths {
		relativeFilePath = ""
		filePath = strings.Split(filePath, "add ")[1]
		out, err := p4CMD("where", filePath)
		if err != nil {
			return nil, errors.Trace(err)
		}
		localPath := strings.Split(strings.TrimSpace(out), " ")[2]
		fileDiff, err := getFileDiff(localPath)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if strings.TrimSpace(fileDiff) == "" {
			return nil, errors.New(fmt.Sprintf("%s No such file, but it has \"add\" action in p4 status", localPath))
		}
		depotFile, err := p4CMD("-Ztag", "-F", "%depotFile%", "where", filePath)
		if err != nil {
			return nil, errors.Trace(err)
		}
		pathElements := strings.Split(strings.Split(strings.TrimSpace(depotFile), "...")[0], "/")
		for i := 4; i < len(pathElements)-1; i++ {
			relativeFilePath += pathElements[i] + "/"
		}
		relativeFilePath += pathElements[len(pathElements)-1]

		patches = append(patches, strings.Replace(fileDiff, strings.Split(localPath, "...")[0], relativeFilePath, 1))
	}

	return patches, nil
}

func getFileDiff(diffPath string) (fileDiff string, err error) {
	data, err := ioutil.ReadFile(diffPath)

	diff := difflib.UnifiedDiff{
		A:        difflib.SplitLines(""),
		B:        difflib.SplitLines(string(data)),
		FromFile: "/dev/null",
		ToFile:   diffPath,
		Context:  0,
	}
	text, _ := difflib.GetUnifiedDiffString(diff)
	return text, nil
}
