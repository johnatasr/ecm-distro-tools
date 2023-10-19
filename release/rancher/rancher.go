package rancher

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v39/github"
	"github.com/rancher/ecm-distro-tools/docker"
	"github.com/rancher/ecm-distro-tools/exec"
	ecmHTTP "github.com/rancher/ecm-distro-tools/http"
	"github.com/rancher/ecm-distro-tools/repository"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

const (
	rancherImagesBaseURL     = "https://github.com/rancher/rancher/releases/download/"
	rancherImagesFileName    = "/rancher-images.txt"
	rancherHelmRepositoryURL = "https://releases.rancher.com/server-charts/latest/index.yaml"

	setKDMBranchReferencesScriptFileName = "set_kdm_branch_references.sh"
	setChartReferencesScriptFileName     = `set_chart_references.sh`
	cloneCheckoutRancherScript           = `#!/bin/sh
set -e

BRANCH_NAME={{ .BranchName }}
DRY_RUN={{ .DryRun }}

cd {{ .RancherRepoPath }}
git remote -v | grep -w upstream || git remote add upstream https://github.com/rancher/rancher.git
git fetch upstream
git stash
git checkout -B ${BRANCH_NAME} upstream/{{.RancherBaseBranch}}
git clean -xfd`
	setKDMBranchReferencesScript = `
OS=$(uname -s)
case ${OS} in
Darwin)
	sed -i '' 's/NewSetting(\"kdm-branch\", \"{{ .CurrentBranch }}\")/NewSetting(\"kdm-branch\", \"{{ .NewBranch }}\")/' pkg/settings/setting.go
	sed -i '' 's/CATTLE_KDM_BRANCH={{ .CurrentBranch }}/CATTLE_KDM_BRANCH={{ .NewBranch }}/' package/Dockerfile
	sed -i '' 's/CATTLE_KDM_BRANCH={{ .CurrentBranch }}/CATTLE_KDM_BRANCH={{ .NewBranch }}/' Dockerfile.dapper
	;;
Linux)
	sed -i 's/NewSetting("kdm-branch", "{{ .CurrentBranch }}")/NewSetting("kdm-branch", "{{ .NewBranch }}")/' pkg/settings/setting.go
	sed -i 's/CATTLE_KDM_BRANCH={{ .CurrentBranch }}/CATTLE_KDM_BRANCH={{ .NewBranch }}/' package/Dockerfile
	sed -i 's/CATTLE_KDM_BRANCH={{ .CurrentBranch }}/CATTLE_KDM_BRANCH={{ .NewBranch }}/' Dockerfile.dapper
	;;
*)
	>&2 echo "$(OS) not supported yet"
	exit 1
	;;
esac
fi
git add pkg/settings/setting.go
git add package/Dockerfile
git add Dockerfile.dapper
git commit --all --signoff -m "update kdm branch to {{ .NewBranch }}"`
	setChartBranchReferencesScript = `
OS=$(uname -s)
case ${OS} in
Darwin)
	sed -i '' 's/NewSetting(\"chart-default-branch\", \"{{ .CurrentBranch }}\")/NewSetting(\"chart-default-branch\", \"{{ .NewBranch }}\")/' pkg/settings/setting.go
	sed -i '' 's/SYSTEM_CHART_DEFAULT_BRANCH={{ .CurrentBranch }}/SYSTEM_CHART_DEFAULT_BRANCH={{ .NewBranch }}/' package/Dockerfile
	sed -i '' 's/CHART_DEFAULT_BRANCH={{ .CurrentBranch }}/CHART_DEFAULT_BRANCH={{ .NewBranch }}/' package/Dockerfile
	sed -i '' 's/{SYSTEM_CHART_DEFAULT_BRANCH:-"{{ .CurrentBranch }}"}/{SYSTEM_CHART_DEFAULT_BRANCH:-"{{ .NewBranch }}"}/' scripts/package-env
	;;
Linux)
	sed -i 's/NewSetting("chart-default-branch", "{{ .CurrentBranch }}")/NewSetting("chart-default-branch", "{{ .NewBranch }}")/' pkg/settings/setting.go
	sed -i 's/SYSTEM_CHART_DEFAULT_BRANCH={{ .CurrentBranch }}/SYSTEM_CHART_DEFAULT_BRANCH={{ .NewBranch }}/' package/Dockerfile
	sed -i 's/CHART_DEFAULT_BRANCH={{ .CurrentBranch }}/CHART_DEFAULT_BRANCH={{ .NewBranch }}/' package/Dockerfile
	sed -i 's/{SYSTEM_CHART_DEFAULT_BRANCH:-"{{ .CurrentBranch }}"}/{SYSTEM_CHART_DEFAULT_BRANCH:-"{{ .NewBranch }}"}/' scripts/package-env
	;;
*)
	>&2 echo "$(OS) not supported yet"
	exit 1
	;;
esac

git add pkg/settings/setting.go
git add package/Dockerfile
git add scripts/package-env
git commit --all --signoff -m "update chart branch references to {{ .NewBranch }}"`
	pushChangesScript = `
if [ "${DRY_RUN}" = false ]; then
	git push --set-upstream origin ${BRANCH_NAME}
fi`
	partialFinalRCCommitMessage = "last commit for final rc"
	releaseTitleRegex           = `^Pre-release v2\.7\.[0-9]{1,100}-rc[1-9][0-9]{0,1}$`
	rancherRepo                 = "rancher"
	rancherOrg                  = rancherRepo
)

type SetBranchReferencesArgs struct {
	RancherRepoPath   string
	CurrentBranch     string
	NewBranch         string
	RancherBaseBranch string
	BranchName        string
	DryRun            bool
}

type HelmIndex struct {
	Entries struct {
		Rancher []struct {
			AppVersion string `yaml:"appVersion"`
		} `yaml:"rancher"`
	} `yaml:"entries"`
}

func ListRancherImagesRC(tag string) (string, error) {
	downloadURL := rancherImagesBaseURL + tag + rancherImagesFileName
	imagesFile, err := rancherImages(downloadURL)
	if err != nil {
		return "", err
	}
	rcImages := nonMirroredRCImages(imagesFile)

	if len(rcImages) == 0 {
		return "There are none non-mirrored images still in rc form for tag " + tag, nil
	}

	output := "The following non-mirrored images for tag *" + tag + "* are still in RC form\n```\n"
	for _, image := range rcImages {
		output += image + "\n"
	}
	output += "```"

	return output, nil
}

func nonMirroredRCImages(images string) []string {
	var rcImages []string

	scanner := bufio.NewScanner(strings.NewReader(images))
	for scanner.Scan() {
		image := scanner.Text()
		if strings.Contains(image, "mirrored") {
			continue
		}
		if strings.Contains(image, "-rc") {
			rcImages = append(rcImages, image)
		}
	}
	return rcImages
}

func rancherImages(imagesURL string) (string, error) {
	httpClient := ecmHTTP.NewClient(time.Second * 15)
	logrus.Debug("downloading: " + imagesURL)
	resp, err := httpClient.Get(imagesURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("failed to download rancher-images.txt file, expected status code 200, got: " + strconv.Itoa(resp.StatusCode))
	}
	images, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(images), nil
}

func CheckRancherDockerImage(ctx context.Context, org, repo, tag string, archs []string) error {
	return docker.CheckImageArchs(ctx, org, repo, tag, archs)
}

func CheckHelmChartVersion(tag string) error {
	versions, err := rancherHelmChartVersions(rancherHelmRepositoryURL)
	if err != nil {
		return err
	}
	var foundVersion bool
	for _, version := range versions {
		logrus.Debug("checking version " + version)
		if tag == version {
			logrus.Info("found chart for version " + version)
			foundVersion = true
			break
		}
	}
	if !foundVersion {
		return errors.New("failed to find chart for rancher app version " + tag)
	}
	return nil
}

func rancherHelmChartVersions(repoURL string) ([]string, error) {
	httpClient := ecmHTTP.NewClient(time.Second * 15)
	logrus.Debug("downloading: " + repoURL)
	resp, err := httpClient.Get(repoURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("failed to download index.yaml file, expected status code 200, got: " + strconv.Itoa(resp.StatusCode))
	}
	var helmIndex HelmIndex
	if err := yaml.NewDecoder(resp.Body).Decode(&helmIndex); err != nil {
		return nil, err
	}
	versions := make([]string, len(helmIndex.Entries.Rancher))
	for i, entry := range helmIndex.Entries.Rancher {
		versions[i] = entry.AppVersion
	}
	return versions, nil
}

func SetKDMBranchReferences(ctx context.Context, forkPath, rancherBaseBranch, currentKDMBranch, newKDMBranch, forkOwner, githubToken string, createPR, dryRun bool) error {
	branchName := "kdm-set-" + newKDMBranch
	data := SetBranchReferencesArgs{
		RancherRepoPath:   forkPath,
		CurrentBranch:     currentKDMBranch,
		NewBranch:         newKDMBranch,
		RancherBaseBranch: rancherBaseBranch,
		DryRun:            dryRun,
		BranchName:        branchName,
	}
	script := cloneCheckoutRancherScript + setKDMBranchReferencesScript + pushChangesScript

	if err := exec.RunTemplatedScript(forkPath, setKDMBranchReferencesScriptFileName, script, data); err != nil {
		return err
	}

	if createPR && !dryRun {
		ghClient := repository.NewGithub(ctx, githubToken)

		if err := createPRFromRancher(ctx, rancherBaseBranch, "Update KDM to "+newKDMBranch, branchName, forkOwner, ghClient); err != nil {
			return err
		}
	}

	return nil
}

func SetChartBranchReferences(ctx context.Context, forkPath, rancherBaseBranch, currentBranch, newBranch, forkOwner, githubToken string, createPR, dryRun bool) error {
	branchName := "charts-set-" + newBranch
	data := SetBranchReferencesArgs{
		RancherRepoPath:   forkPath,
		CurrentBranch:     currentBranch,
		NewBranch:         newBranch,
		RancherBaseBranch: rancherBaseBranch,
		DryRun:            dryRun,
		BranchName:        branchName,
	}
	script := cloneCheckoutRancherScript + setChartBranchReferencesScript + pushChangesScript
	if err := exec.RunTemplatedScript(forkPath, setChartReferencesScriptFileName, script, data); err != nil {
		return err
	}

	if createPR && !dryRun {
		ghClient := repository.NewGithub(ctx, githubToken)

		if err := createPRFromRancher(ctx, rancherBaseBranch, "Update charts branch references to "+newBranch, branchName, forkOwner, ghClient); err != nil {
			return err
		}
	}

	return nil
}

func createPRFromRancher(ctx context.Context, rancherBaseBranch, title, branchName, forkOwner string, ghClient *github.Client) error {
	org, err := repository.OrgFromRepo(rancherRepo)
	if err != nil {
		return err
	}
	pull := &github.NewPullRequest{
		Title:               github.String(title),
		Base:                github.String(rancherBaseBranch),
		Head:                github.String(forkOwner + ":" + branchName),
		MaintainerCanModify: github.Bool(true),
	}
	_, _, err = ghClient.PullRequests.Create(ctx, org, rancherRepo, pull)

	return err
}

func CheckRancherFinalRCDeps(org, repo, commitHash, releaseTitle, files string) error {
	var (
		devDependencyPattern = regexp.MustCompile(`dev-v[0-9]+\.[0-9]+`)
		rcTagPattern         = regexp.MustCompile(`-rc[0-9]+`)
		matchCommitMessage   bool
		existsReleaseTitle   bool
		badFiles             bool
	)

	httpClient := ecmHTTP.NewClient(time.Second * 15)

	if repo == "" {
		repo = rancherRepo
	}
	if commitHash != "" {
		commitData, err := repository.CommitInfo(org, repo, commitHash, &httpClient)
		if err != nil {
			return err
		}
		matchCommitMessage = strings.Contains(commitData.Message, partialFinalRCCommitMessage)

	}
	if releaseTitle != "" {
		innerExistsReleaseTitle, err := regexp.MatchString(releaseTitleRegex, releaseTitle)
		if err != nil {
			return err
		}
		existsReleaseTitle = innerExistsReleaseTitle
	}

	if matchCommitMessage || existsReleaseTitle {
		for _, filePath := range strings.Split(files, ",") {
			content, err := repository.ContentByFileNameAndCommit(org, repo, commitHash, filePath, &httpClient)
			if err != nil {
				return err
			}

			if devDependencyPattern.Match(content) {
				badFiles = true
				logrus.Info("error: " + filePath + " contains dev dependencies")
			}
			if rcTagPattern.Match(content) {
				badFiles = true
				logrus.Info("error: " + filePath + " contains rc tags")
			}
		}
		if badFiles {
			return errors.New("check failed, some files don't match the expected dependencies for a final release candidate")
		}

		logrus.Info("check completed successfully")
		return nil
	}

	logrus.Info("skipped check")
	return nil
}
