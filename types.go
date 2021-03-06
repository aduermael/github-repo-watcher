package main

import (
	"errors"
	"net/url"
	"os"
	"os/exec"
	"strings"

	git "gopkg.in/src-d/go-git.v4"
	gitConfig "gopkg.in/src-d/go-git.v4/config"
	gitPlumbing "gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/transport/http"
)

// WatchConfig is the configuration used to watch Github repositories
type WatchConfig struct {
	Repos map[string]*Repo `yaml:"repos"`
}

// Repo represents a watched Github repository
type Repo struct {
	Name     string             `yaml:"-"`
	URL      string             `yaml:"url"`
	Branches map[string]*Branch `yaml:"branches"`
	gitRepo  *git.Repository
}

// Branch contains what needs to be watched in the git branch
// If no specific filters are set, then any change will be reported
type Branch struct {
	Name string `yaml:"-"`
	// Commit represents the last commit that's been processed
	Commit string `yaml:"commit,omitempty"`
	// if non empty, only listed files (or directories) will be watched
	Files []string `yaml:"files,omitempty"`
}

// GetBranchIfTracked returns *Branch corresponding to refName if listed in
// the config. Otherwise it returns nil
// (refName provided should be the short version)
func (r *Repo) GetBranchIfTracked(refName string) *Branch {
	branchName := strings.TrimPrefix(refName, "origin/")
	for name, branch := range r.Branches {
		if name == branchName {
			return branch
		}
	}
	return nil
}

func (r *Repo) storageDir() string {
	return reposDir + "/" + r.Name
}

func (r *Repo) openOrInitGitRepo() error {

	var err error

	r.gitRepo, err = git.PlainOpen(r.storageDir())

	// repo found and opened, but is it the one we're looking for?
	// we should check the URL and delete directory if it doesn't match
	for err == nil {
		var remotes []*git.Remote
		remotes, err = r.gitRepo.Remotes()

		if err != nil {
			rmDir(r.storageDir())
			break
		}

		// TODO: allow several remotes
		if len(remotes) > 1 {
			err = errors.New("only one remote expected")
			rmDir(r.storageDir())
			break
		}

		if r.URL != remotes[0].Config().URL {
			err = errors.New("remote URL is different from the one in the config")
			debug(err)
			rmDir(r.storageDir())
			break
		}

		break
	}

	if err != nil {
		// if the repo does not exist, init & create remote (no need to clone)
		if err == git.ErrRepositoryNotExists {
			r.gitRepo, err = git.PlainInit(r.storageDir(), true)
			if err != nil {
				return err
			}
			// TODO: allow different remotes?
			// repo.fetch fetches "origin" by default
			// var remote *git.Remote
			_, err = r.gitRepo.CreateRemote(&gitConfig.RemoteConfig{Name: "origin", URL: r.URL})

			// initial fetch because we just added the remote
			debug("initial fetch")
			err = r.fetch()
			if err != nil {
				return err
			}
		}
		if err != nil {
			return err
		}

		// save current ref hash from each branch we're watching
		// (the configuration file will be updated with these commits)
		referencesIter, err := r.gitRepo.References()
		if err != nil {
			return err
		}
		referencesIter.ForEach(func(ref *gitPlumbing.Reference) error {
			// only consider remotes
			if ref.IsRemote() {
				branch := r.GetBranchIfTracked(ref.Name().Short())
				if branch != nil {
					branch.Commit = ref.Hash().String()
				}
			}
			return nil
		})
	}

	return nil
}

func (r *Repo) fetch() error {
	// check url domain and env variables to see if Github token should be used
	u, err := url.Parse(r.URL)
	if err != nil {
		debug(err)
		return err
	}
	if u.Host == "github.com" && os.Getenv("GITHUB_USER") != "" && os.Getenv("GITHUB_TOKEN") != "" {
		err = r.gitRepo.Fetch(&git.FetchOptions{Auth: http.NewBasicAuth(os.Getenv("GITHUB_USER"), os.Getenv("GITHUB_TOKEN"))})
	} else {
		err = r.gitRepo.Fetch(&git.FetchOptions{})
	}
	return err
}

func (r *Repo) fetchAndLookForChanges() error {

	if r.gitRepo == nil {
		return errors.New("git repo not opened")
	}

	err := r.fetch()

	if err != nil && err != git.NoErrAlreadyUpToDate {
		debug(err)
		// TODO: an error here may be due to a force push
		// we can just reset head and generate a message
		return err
	}

	debug("fetched", r.URL)

	// check references
	referencesIter, err := r.gitRepo.References()
	if err != nil {
		return err
	}

	err = referencesIter.ForEach(func(ref *gitPlumbing.Reference) error {
		// only consider remotes
		if ref.IsRemote() {
			branch := r.GetBranchIfTracked(ref.Name().Short())
			if branch != nil {
				// commit has changed!
				// look for changes...
				if branch.Commit != ref.Hash().String() {
					debug(branch.Commit, "!=", ref.Hash().String())

					// this means we certainly fetch for the first time
					// we don't have anything to compare, so let's just
					// save the reference and return
					if branch.Commit == "" {
						branch.Commit = ref.Hash().String()
						return nil
					}

					wd, err := os.Getwd()
					if err != nil {
						return err
					}
					os.Chdir(r.storageDir())
					cmd := exec.Command("git", "diff", branch.Commit, ref.Hash().String(), "--name-status")
					output, err := cmd.Output()
					if err != nil {
						return err
					}

					diffs := parseDiffOutput(output)
					for _, diff := range diffs {
						debug(diff.Type, "-", diff.File)
					}
					os.Chdir(wd)

					report := true

					// see if we actually need to report, we may not be
					// interested in changes depending on the files affected
					for len(branch.Files) > 0 {
						report = false
						for _, filePattern := range branch.Files {
							for _, diff := range diffs {
								if filePathMatchPattern(filePattern, diff.File) {
									report = true
									break
								}
							}
						}
						break
					}

					if report {
						title := r.Name + " (" + branch.Commit[:8] + " .. " + ref.Hash().String()[:8] + ")"
						description := "Changes in " + r.Name + " (<a href=\"" + r.URL + "\">" + r.URL + "</a>)<br><br>" +
							"<b>" + branch.Commit + "</b><br>"
						for _, diff := range diffs {
							description += string(diff.Type) + " - " + diff.File + "<br>"
						}
						description += "<b>" + ref.Hash().String() + "</b>"
						newFeedItem(title, description, r.URL)
					}

					branch.Commit = ref.Hash().String()
				}
			}
		}
		return nil
	})

	return err
}
