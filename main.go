// Command git-cheat-leadtime rebases the current branch onto the remote's main
// branch and resets commit dates to now, so commits look like they were just
// made. Rebase conflicts are handed to claude to resolve.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const usage = `usage: git-cheat-leadtime [skip=fix] [base=BRANCH] [remote=NAME] [push] [newpr] [DIR]
  skip=fix  don't ask claude to resolve conflicts; stop the rebase for manual fixing
  base      branch to rebase onto (default: the remote's HEAD, else main or master)
  remote    remote to fetch from (default: origin)
  push      force push (with lease) when done
  newpr     push, then close the branch's open PR and open a new one in its
            place (same title, body, base, labels, assignees, reviewers) with
            a comment on the old PR linking to the new one; needs gh
  DIR       repo to update (default: .)
options may also be written -opt=VALUE or --opt=VALUE

Fetches BASE, then rebases the current branch onto it with every commit's
date reset to now. If BASE has nothing new, the commits are still rewritten
in place so they get today's date.
`

type options struct {
	skip   map[string]bool
	base   string
	remote string
	push   bool
	newpr  bool
	dir    string
}

func main() {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fatal(err)
	}
	if opts == nil { // help
		fmt.Print(usage)
		return
	}
	if err := run(opts); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "git-cheat-leadtime:", err)
	os.Exit(1)
}

// parseArgs returns nil options (and nil error) when help was requested.
func parseArgs(args []string) (*options, error) {
	opts := &options{dir: ".", remote: "origin", skip: map[string]bool{}}
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return nil, nil
		}
		key, val, hasEq := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if hasEq {
			switch key {
			case "skip":
				opts.skip = map[string]bool{}
				for s := range strings.SplitSeq(val, ",") {
					opts.skip[strings.TrimSpace(s)] = true
				}
				continue
			case "base":
				opts.base = val
				continue
			case "remote":
				opts.remote = val
				continue
			case "push":
				opts.push = val != "false" && val != "0"
				continue
			case "newpr":
				opts.newpr = val != "false" && val != "0"
				continue
			}
		} else if key == "push" {
			opts.push = true
			continue
		} else if key == "newpr" {
			opts.newpr = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return nil, fmt.Errorf("unknown option %s", arg)
		}
		opts.dir = arg
	}

	info, err := os.Stat(opts.dir)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("not a directory: %s", opts.dir)
	}
	abs, err := filepath.Abs(opts.dir)
	if err != nil {
		return nil, err
	}
	opts.dir = abs
	if opts.newpr {
		opts.push = true // the new PR needs the rewritten branch on the remote
	}
	return opts, nil
}

func run(opts *options) error {
	dir := opts.dir
	if _, err := git(dir, "rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("not a git repo: %s", dir)
	}
	if rebaseInProgress(dir) {
		return fmt.Errorf("a rebase is already in progress; finish it or run git rebase --abort")
	}
	branch, _ := git(dir, "symbolic-ref", "--short", "-q", "HEAD")
	if branch == "" {
		return fmt.Errorf("detached HEAD; check out a branch first")
	}
	if st, _ := git(dir, "status", "--porcelain", "--untracked-files=no"); st != "" {
		return fmt.Errorf("uncommitted changes; commit or stash them first")
	}

	// Look up the PR before rewriting anything, so a missing PR or gh
	// problem fails fast instead of after the rebase.
	var oldPR *pullRequest
	if opts.newpr {
		pr, err := findPR(dir, branch)
		if err != nil {
			return err
		}
		oldPR = pr
		fmt.Printf("found PR #%d: %s\n", pr.Number, pr.Title)
	}

	base := opts.base
	if base == "" {
		base = defaultBranch(dir, opts.remote)
		if base == "" {
			return fmt.Errorf("can't find the main branch of %s; pass base=BRANCH", opts.remote)
		}
	}
	if branch == base {
		return fmt.Errorf("on %s itself; check out a feature branch", base)
	}

	fmt.Printf("fetching %s/%s\n", opts.remote, base)
	if err := gitInteractive(dir, "fetch", opts.remote, base); err != nil {
		return fmt.Errorf("fetch failed: %v", err)
	}
	upstream := opts.remote + "/" + base

	count, _ := git(dir, "rev-list", "--count", upstream+"..HEAD")
	if count == "0" {
		return fmt.Errorf("%s has no commits that aren't in %s", branch, upstream)
	}
	if _, err := git(dir, "merge-base", "--is-ancestor", upstream, "HEAD"); err == nil {
		fmt.Printf("%s has nothing new; redating %s commit(s) on %s\n", upstream, count, branch)
	} else {
		fmt.Printf("rebasing %s commit(s) on %s onto %s\n", count, branch, upstream)
	}

	// --force-rebase rewrites the commits even when there is nothing to
	// rebase, and --reset-author-date sets their author date to now (the
	// committer date is always now for rewritten commits).
	err := gitInteractive(dir, "rebase", "--force-rebase", "--reset-author-date", upstream)
	for err != nil {
		if !rebaseInProgress(dir) {
			return fmt.Errorf("rebase failed: %v", err)
		}
		files := conflictedFiles(dir)
		if len(files) == 0 {
			return fmt.Errorf("rebase stopped without conflicts; finish it with git rebase --continue or --abort")
		}
		if opts.skip["fix"] {
			return fmt.Errorf("conflicts in %s; resolve them, then git add and git rebase --continue (or git rebase --abort)",
				strings.Join(files, ", "))
		}
		if ferr := fixWithClaude(dir, upstream, files); ferr != nil {
			return fmt.Errorf("%v; resolve the rest yourself, then git add and git rebase --continue (or git rebase --abort)", ferr)
		}
		err = gitInteractive(dir, "rebase", "--continue")
	}

	if opts.push {
		fmt.Println("pushing")
		if err := gitInteractive(dir, "push", "--force-with-lease", opts.remote, branch); err != nil {
			return fmt.Errorf("push failed: %v", err)
		}
	}
	if oldPR != nil {
		if err := replacePR(dir, branch, oldPR); err != nil {
			return err
		}
	}
	fmt.Printf("done: %s is on %s with today's dates\n", branch, upstream)
	return nil
}

// defaultBranch returns the remote's HEAD branch, falling back to main or
// master, or "" if none can be found.
func defaultBranch(dir, remote string) string {
	if ref, err := git(dir, "symbolic-ref", "--short", "-q", "refs/remotes/"+remote+"/HEAD"); err == nil && ref != "" {
		return strings.TrimPrefix(ref, remote+"/")
	}
	// Ask the remote directly; this works even when origin/HEAD was never set.
	if out, err := git(dir, "ls-remote", "--symref", remote, "HEAD"); err == nil {
		for line := range strings.SplitSeq(out, "\n") {
			if rest, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
				name, _, _ := strings.Cut(rest, "\t")
				return name
			}
		}
	}
	for _, name := range []string{"main", "master"} {
		if _, err := git(dir, "rev-parse", "--verify", "-q", "refs/remotes/"+remote+"/"+name); err == nil {
			return name
		}
	}
	return ""
}

// fixWithClaude asks claude to resolve the conflicted files, then stages them.
// It fails if claude errors or leaves conflict markers behind.
func fixWithClaude(dir, upstream string, files []string) error {
	commit, _ := git(dir, "log", "-1", "--format=%h %s", "REBASE_HEAD")
	fmt.Printf("conflicts in %s; asking claude to resolve them\n", strings.Join(files, ", "))

	prompt := fmt.Sprintf(`A git rebase onto %s stopped with merge conflicts while applying commit %q.
Resolve the conflicts in these files: %s

Keep the intent of both sides: the upstream changes and the changes from the commit being applied.
Remove every conflict marker (<<<<<<<, =======, >>>>>>>). Only edit the conflicted files.
Do not run git add, git commit, or git rebase; that is handled for you.
Use git log, git show, and git diff to understand each side if needed.`,
		upstream, commit, strings.Join(files, ", "))

	c := exec.Command("claude", "-p", prompt,
		"--permission-mode", "acceptEdits",
		"--allowedTools", "Read,Edit,Write,Grep,Glob,Bash(git diff:*),Bash(git log:*),Bash(git show:*),Bash(git status:*)",
	)
	c.Dir = dir
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("claude failed: %v", err)
	}

	var unresolved []string
	for _, f := range files {
		if hasConflictMarkers(filepath.Join(dir, f)) {
			unresolved = append(unresolved, f)
		}
	}
	if len(unresolved) > 0 {
		return fmt.Errorf("claude left conflict markers in %s", strings.Join(unresolved, ", "))
	}
	// -A stages deletions too, in case claude resolved a conflict by removing a file.
	if _, err := git(dir, append([]string{"add", "-A", "--"}, files...)...); err != nil {
		return fmt.Errorf("git add failed: %v", err)
	}
	return nil
}

func hasConflictMarkers(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false // deleted
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, len(data)+1)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "<<<<<<< ") || strings.HasPrefix(line, ">>>>>>> ") {
			return true
		}
	}
	return false
}

func conflictedFiles(dir string) []string {
	out, _ := git(dir, "diff", "--name-only", "--diff-filter=U")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func rebaseInProgress(dir string) bool {
	gitdir, err := git(dir, "rev-parse", "--absolute-git-dir")
	if err != nil || gitdir == "" {
		return false
	}
	return exists(filepath.Join(gitdir, "rebase-merge")) || exists(filepath.Join(gitdir, "rebase-apply"))
}

func git(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimRight(string(out), "\n"), err
}

// gitInteractive runs git with its output shown to the user. The editor is
// disabled so rebase --continue keeps the original commit messages.
func gitInteractive(dir string, args ...string) error {
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(), "GIT_EDITOR=true")
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

type pullRequest struct {
	Number      int    `json:"number"`
	URL         string `json:"url"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	BaseRefName string `json:"baseRefName"`
	IsDraft     bool   `json:"isDraft"`
	Author      struct {
		Login string `json:"login"`
	} `json:"author"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Assignees []struct {
		Login string `json:"login"`
	} `json:"assignees"`
	ReviewRequests []struct {
		Login string `json:"login"` // empty for team requests
	} `json:"reviewRequests"`
	LatestReviews []struct {
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
	} `json:"latestReviews"`
}

// findPR returns the open PR whose head is branch.
func findPR(dir, branch string) (*pullRequest, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, fmt.Errorf("newpr needs the gh CLI: https://cli.github.com")
	}
	out, err := gh(dir, "pr", "view", branch, "--json",
		"number,url,title,body,baseRefName,isDraft,state,author,labels,assignees,reviewRequests,latestReviews")
	if err != nil {
		return nil, fmt.Errorf("no PR found for %s: %v", branch, err)
	}
	var pr struct {
		pullRequest
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &pr); err != nil {
		return nil, fmt.Errorf("can't read gh output: %v", err)
	}
	if pr.State != "OPEN" {
		return nil, fmt.Errorf("PR #%d for %s is %s, not open", pr.Number, branch, strings.ToLower(pr.State))
	}
	return &pr.pullRequest, nil
}

// replacePR closes old and opens a new PR from branch with the same details,
// then comments on old with a link to the new one. GitHub allows only one open
// PR per branch and base, so old has to be closed first; if creating the new
// one fails, old is reopened.
func replacePR(dir, branch string, old *pullRequest) error {
	num := fmt.Sprint(old.Number)
	fmt.Printf("closing PR #%d\n", old.Number)
	if _, err := gh(dir, "pr", "close", num); err != nil {
		return fmt.Errorf("closing PR #%d failed: %v", old.Number, err)
	}

	body := strings.TrimRight(old.Body, "\n")
	if body != "" {
		body += "\n\n"
	}
	body += fmt.Sprintf("Replaces #%d.", old.Number)

	args := []string{"pr", "create", "--head", branch, "--base", old.BaseRefName,
		"--title", old.Title, "--body", body}
	if old.IsDraft {
		args = append(args, "--draft")
	}
	for _, l := range old.Labels {
		args = append(args, "--label", l.Name)
	}
	for _, a := range old.Assignees {
		args = append(args, "--assignee", a.Login)
	}
	// Re-request everyone who reviewed or was asked to; their reviews stay on
	// the old PR.
	seen := map[string]bool{old.Author.Login: true, "": true}
	addReviewer := func(login string) {
		if !seen[login] {
			seen[login] = true
			args = append(args, "--reviewer", login)
		}
	}
	for _, r := range old.LatestReviews {
		addReviewer(r.Author.Login)
	}
	for _, r := range old.ReviewRequests {
		addReviewer(r.Login)
	}

	fmt.Println("opening new PR")
	newURL, err := gh(dir, args...)
	if err != nil {
		if _, rerr := gh(dir, "pr", "reopen", num); rerr != nil {
			return fmt.Errorf("creating the new PR failed (%v) and reopening #%d failed too (%v)", err, old.Number, rerr)
		}
		return fmt.Errorf("creating the new PR failed, so #%d was reopened: %v", old.Number, err)
	}
	newURL = lastLine(newURL)

	if _, err := gh(dir, "pr", "comment", num, "--body", "Replaced by "+newURL); err != nil {
		fmt.Fprintf(os.Stderr, "git-cheat-leadtime: warning: commenting on #%d failed: %v\n", old.Number, err)
	}
	fmt.Printf("replaced %s with %s\n", old.URL, newURL)
	return nil
}

// gh runs the gh CLI in dir. On failure the error includes gh's stderr.
func gh(dir string, args ...string) (string, error) {
	c := exec.Command("gh", args...)
	c.Dir = dir
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%v: %s", err, msg)
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
