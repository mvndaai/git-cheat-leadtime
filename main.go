// Command git-cheat-leadtime rebases the current branch onto the remote's main
// branch and resets commit dates to now, so commits look like they were just
// made. Rebase conflicts are handed to claude to resolve.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const usage = `usage: git-cheat-leadtime [skip=fix] [base=BRANCH] [remote=NAME] [push] [DIR]
  skip=fix  don't ask claude to resolve conflicts; stop the rebase for manual fixing
  base      branch to rebase onto (default: the remote's HEAD, else main or master)
  remote    remote to fetch from (default: origin)
  push      force push (with lease) when done
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
			}
		} else if key == "push" {
			opts.push = true
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
