// Copyright 2016 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// sci is a shameful CI.
//
// It is a simple Github webhook that runs a Go build and an hardcoded
// command upon PR or push from whitelisted users.
//
// It posts the stdout to a Github gist and updates the PR status.
//
// It doesn't stream data so it cannot be used for slow task.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bugsnag/osext"
	"github.com/google/go-github/github"
	"golang.org/x/oauth2"
)

type config struct {
	Port              int        // TCP port number for HTTP server.
	WebHookSecret     string     // https://developer.github.com/webhooks/
	Oauth2AccessToken string     // https://github.com/settings/tokens, check "repo:status" and "gist"
	UseSSH            bool       // Use ssh (instead of https) for checkout. Required for private repositories.
	Name              string     // Display name to use in the status report on Github.
	Checks            [][]string // Commands to run to test the repository. They are run one after the other from the repository's root.
}

func loadConfig() (*config, error) {
	c := &config{
		Port:              8080,
		WebHookSecret:     "Create a secret and set it at github.com/'name'/'repo'/settings/hooks",
		Oauth2AccessToken: "Get one at https://github.com/settings/tokens",
		UseSSH:            false,
		Name:              "sci",
		Checks:            [][]string{{"go", "test", "./..."}},
	}
	b, err := ioutil.ReadFile("sci.json")
	if err != nil {
		b, err = json.MarshalIndent(c, "", "  ")
		if err != nil {
			return nil, err
		}
		if err = ioutil.WriteFile("sci.json", b, 0600); err != nil {
			return nil, err
		}
		return nil, errors.New("wrote new sci.json")
	}
	if err = json.Unmarshal(b, c); err != nil {
		return nil, err
	}
	d, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(b, d) {
		log.Printf("Updating sci.json in canonical format")
		if err := ioutil.WriteFile("sci.json", d, 0600); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func run(cwd string, cmd ...string) (string, bool) {
	cmds := strings.Join(cmd, " ")
	log.Printf("- cwd=%s : %s", cwd, cmds)
	c := exec.Command(cmd[0], cmd[1:]...)
	c.Dir = cwd
	start := time.Now()
	out, err := c.CombinedOutput()
	duration := time.Since(start)
	// Assumes UTF-8.
	return fmt.Sprintf("$ %s  (in %s)%s", cmds, duration, string(out)), err == nil
}

// runChecks syncs then runs the checks and returns task's results.
func runChecks(cmds [][]string, repoName string, useSSH bool, commit, gopath string) (map[string]string, bool) {
	out := map[string]string{
		"metadata": fmt.Sprintf(
			"Commit: %s\nVersion: %s\nGOROOT: %s\nGOPATH: %s\nCPUs: %d",
			commit, runtime.Version(), runtime.GOROOT(), gopath, runtime.NumCPU()),
		"setup": "",
	}
	repoPath := "github.com/" + repoName
	base := filepath.Join(gopath, "src", repoPath)
	if _, err := os.Stat(base); err != nil {
		up := path.Dir(base)
		if err := os.MkdirAll(up, 0700); err != nil && !os.IsExist(err) {
			log.Printf("- %v", err)
		}
		url := "https://" + repoPath
		if useSSH {
			url = "git@github.com:" + repoName
		}
		stdout, ok := run(up, "git", "clone", "--quiet", url)
		out["setup"] = stdout
		if !ok {
			return out, ok
		}
	} else {
		stdout, ok := run(base, "git", "fetch", "--prune", "--quiet")
		out["setup"] = stdout
		if !ok {
			return out, ok
		}
	}
	stdout, ok := run(base, "git", "checkout", "--quiet", commit)
	out["setup"] += stdout
	if ok {
		// TODO(maruel): update dependencies manually!
		stdout, ok = run(base, "go", "get", "-v", "-d", "-t", "./...")
		out["setup"] += stdout
		if ok {
			// Precompilation has a dramatic effect on a Raspberry Pi.
			stdout, ok = run(base, "go", "test", "-i", "./...")
			out["setup"] += stdout
			if ok {
				// Finally run the checks!
				for i, cmd := range cmds {
					ok2 := true
					if out[fmt.Sprintf("cmd%d", i+1)], ok2 = run(base, cmd...); !ok2 {
						ok = false
					}
				}
			}
		}
	}
	return out, ok
}

type server struct {
	c       *config
	client  *github.Client
	gopath  string
	mu      sync.Mutex
	collabs map[string]map[string]bool
}

func (s *server) canCollab(owner, repo, user string) bool {
	key := owner + "/" + repo
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.collabs[key]; !ok {
		s.collabs[key] = map[string]bool{}
	}
	if v, ok := s.collabs[key][user]; ok {
		return v
	}
	v, _, _ := s.client.Repositories.IsCollaborator(owner, repo, user)
	if v {
		// Only cache hits because otherwise adding a collaborator would mean
		// restarting every sci instances.
		s.collabs[key][user] = v
	}
	log.Printf("- %s: %s access: %t", key, user, v)
	return v
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("HTTP: %s %s", r.RemoteAddr, r.URL.Path)
	defer r.Body.Close()
	if r.Method != "POST" {
		http.Error(w, "Invalid method", http.StatusMethodNotAllowed)
		log.Printf("- invalid method")
		return
	}
	payload, err := github.ValidatePayload(r, []byte(s.c.WebHookSecret))
	if err != nil {
		http.Error(w, "Invalid secret", http.StatusUnauthorized)
		log.Printf("- invalid secret")
		return
	}
	if t := github.WebHookType(r); t != "ping" {
		event, err := github.ParseWebHook(t, payload)
		if err != nil {
			http.Error(w, "Invalid payload", http.StatusBadRequest)
			log.Printf("- invalid payload")
			return
		}
		// Process the rest asynchronously so the hook doesn't take too long.
		go func() {
			switch event := event.(type) {
			// TODO(maruel): For *github.CommitCommentEvent and
			// *github.IssueCommentEvent, when the comment is 'run tests' from a
			// collaborator, run the tests.
			case *github.PullRequestEvent:
				log.Printf("- PR %s #%d %s %s", *event.Repo.FullName, *event.PullRequest.ID, *event.Sender.Login, *event.Action)
				if *event.Action != "opened" && *event.Action != "synchronized" {
					log.Printf("- ignoring action %q for PR from %q", *event.Action, *event.Sender.Login)
				} else if !s.canCollab(*event.Repo.Owner.Login, *event.Repo.Name, *event.Sender.Login) {
					log.Printf("- ignoring owner %q for PR", *event.Sender.Login)
				} else if err = s.runCheck(*event.Repo.FullName, *event.PullRequest.Head.SHA); err != nil {
					log.Printf("- %v", err)
				}
			case *github.PushEvent:
				if event.HeadCommit == nil {
					log.Printf("- Push %s %s <deleted>", *event.Repo.FullName, *event.Ref)
				} else {
					log.Printf("- Push %s %s %s", *event.Repo.FullName, *event.Ref, *event.HeadCommit.ID)
					if !strings.HasPrefix(*event.Ref, "refs/heads/") {
						log.Printf("- ignoring branch %q for push", *event.Ref)
					} else if err = s.runCheck(*event.Repo.FullName, *event.HeadCommit.ID); err != nil {
						log.Printf("- %v", err)
					}
				}
			default:
				log.Printf("- ignoring hook type %s", reflect.TypeOf(event).Elem().Name())
			}
		}()
	}
	io.WriteString(w, "{}")
}

func (s *server) runCheck(repo, commit string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	log.Printf("- Running test for %s at %s", repo, commit)
	// TODO(maruel): Update the gist as the task is running;
	// https://developer.github.com/v3/gists/#edit-a-gist
	out, success := runChecks(s.c.Checks, repo, s.c.UseSSH, commit, s.gopath)
	// https://developer.github.com/v3/gists/#create-a-gist
	// It is still accessible via the URL without authentication.
	gist := &github.Gist{
		Description: github.String("Output for https://github.com/" + repo + "/commit/" + commit),
		Public:      github.Bool(false),
		Files:       map[github.GistFilename]github.GistFile{},
	}
	for k, v := range out {
		if len(v) == 0 {
			v = "<missing>"
		}
		gist.Files[github.GistFilename(k)] = github.GistFile{Content: github.String(v)}
	}
	var err error
	if gist, _, err = s.client.Gists.Create(gist); err != nil {
		return err
	}
	log.Printf("- Gist at %s", *gist.HTMLURL)

	// https://developer.github.com/v3/repos/statuses/#create-a-status
	desc := "Ran:\n"
	for i, c := range s.c.Checks {
		if i != 0 {
			desc += "\n"
		}
		desc += "  " + strings.Join(c, " ")
	}
	status := &github.RepoStatus{
		State:       github.String("success"),
		TargetURL:   gist.HTMLURL,
		Description: &desc,
		Context:     github.String("sci"),
	}
	if !success {
		status.State = github.String("failure")
	}
	parts := strings.SplitN(repo, "/", 2)
	_, _, err = s.client.Repositories.CreateStatus(parts[0], parts[1], commit, status)
	return err
}

func mainImpl() error {
	test := flag.String("test", "", "runs a simulation locally, specify the git repository name (not URL) to test, e.g. 'maruel/sci'")
	commit := flag.String("commit", "HEAD", "commit ID to test and update; will only update if not 'HEAD'")
	flag.Parse()
	c, err := loadConfig()
	if err != nil {
		return err
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	gopath := filepath.Join(wd, "sci-gopath")
	os.Setenv("GOPATH", gopath)
	tc := oauth2.NewClient(oauth2.NoContext, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: c.Oauth2AccessToken}))
	s := server{c: c, client: github.NewClient(tc), gopath: gopath, collabs: map[string]map[string]bool{}}
	if len(*test) != 0 {
		if *commit == "HEAD" {
			// Only run locally.
			out, success := runChecks(c.Checks, *test, c.UseSSH, *commit, gopath)
			names := make([]string, 0, len(out))
			for k := range out {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				fmt.Printf("--- %s\n%s", k, out[k])
			}
			_, err := fmt.Printf("\nSuccess: %t\n", success)
			return err
		}
		return s.runCheck(*test, *commit)
	}
	http.Handle("/", &s)
	thisFile, err := osext.Executable()
	if err != nil {
		return err
	}
	log.Printf("Running in: %s", wd)
	log.Printf("Executable: %s", thisFile)
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", c.Port))
	if err != nil {
		return err
	}
	a := ln.Addr().String()
	ln.Close()
	log.Printf("Listening on: %s", a)
	go http.ListenAndServe(a, nil)
	// TODO(maruel): watch sci.json too.
	err = watchFile(thisFile)
	// Ensures no task is running.
	s.mu.Lock()
	return err
}

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "sci: %s.\n", err)
		os.Exit(1)
	}
}
