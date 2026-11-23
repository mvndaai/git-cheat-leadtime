# git-cheat-leadtime
My company likes low lead time, so this lets me update commits to look like they were made right now instead of when they were created

## Install

```sh
go install github.com/mvndaai/git-cheat-leadtime@latest
```

Because the binary is named `git-cheat-leadtime`, you can run it as `git-cheat-leadtime` or `git cheat-leadtime`.

## Usage

```
git-cheat-leadtime [skip=fix] [base=BRANCH] [remote=NAME] [push] [newpr] [branch=NAME] [DIR]
  skip=fix  don't ask claude to resolve conflicts; stop the rebase for manual fixing
  base      branch to rebase onto (default: the remote's HEAD, else main or master)
  remote    remote to fetch from (default: origin)
  push      force push (with lease) when done
  newpr     move the commits to a new branch, push it and open a new PR in
            place of the branch's open PR (same title, body, base, labels,
            assignees, reviewers), then comment on and close the old PR and
            delete the old branch; needs gh
  branch    name of the new branch for newpr (default: BRANCH-YYYYMMDD)
  DIR       repo to update (default: .)
options may also be written -opt=VALUE or --opt=VALUE
```

It fetches the main branch and rebases the current branch onto it, resetting every commit's date to now.
If main has nothing new, the commits are still rewritten in place so they get today's date.

If the rebase hits conflicts, [Claude Code](https://claude.com/claude-code) (`claude -p`) is asked to resolve them.
It may only edit files and read git history. The tool then checks for leftover conflict markers, stages the files and continues the rebase.
If Claude fails, or with `skip=fix`, the rebase is left in progress so you can finish it yourself.

With `newpr`, the commits move to a new branch with a new PR, so the branch and the PR's opened date are new as well (GitHub doesn't allow changing a PR's opened date).
The local branch is renamed to the new name (`BRANCH-YYYYMMDD` by default, or `branch=NAME`) and pushed.
The new PR copies the title, body, base, draft state, labels and assignees, links back with "Replaces #N", and asks everyone who reviewed the old PR to review again.
Only after the new PR exists is the old PR closed with a "Replaced by" comment and the old branch deleted from the remote (GitHub only auto-deletes branches on merge, not on close). The deletion is checked, and if `git push --delete` doesn't remove it the GitHub API is used instead. If anything fails before the new PR exists, the old PR is left alone.
Approvals don't carry over, because reviews belong to a PR.
