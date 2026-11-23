# git-cheat-leadtime
My company likes low lead time, so this lets me update commits to look like they were made right now instead of when they were created

## Install

```sh
go install github.com/mvndaai/git-cheat-leadtime@latest
```

Because the binary is named `git-cheat-leadtime`, you can run it as `git-cheat-leadtime` or `git cheat-leadtime`.

## Usage

```
git-cheat-leadtime [skip=fix] [base=BRANCH] [remote=NAME] [push] [newpr] [DIR]
  skip=fix  don't ask claude to resolve conflicts; stop the rebase for manual fixing
  base      branch to rebase onto (default: the remote's HEAD, else main or master)
  remote    remote to fetch from (default: origin)
  push      force push (with lease) when done
  newpr     push, then close the branch's open PR and open a new one in its
            place (same title, body, base, labels, assignees, reviewers) with
            a comment on the old PR linking to the new one; needs gh
  DIR       repo to update (default: .)
options may also be written -opt=VALUE or --opt=VALUE
```

It fetches the main branch and rebases the current branch onto it, resetting every commit's date to now.
If main has nothing new, the commits are still rewritten in place so they get today's date.

If the rebase hits conflicts, [Claude Code](https://claude.com/claude-code) (`claude -p`) is asked to resolve them.
It may only edit files and read git history. The tool then checks for leftover conflict markers, stages the files and continues the rebase.
If Claude fails, or with `skip=fix`, the rebase is left in progress so you can finish it yourself.

With `newpr`, the branch's open PR is closed and replaced by a new one, so the PR's opened date is today as well (GitHub doesn't allow changing it).
The new PR copies the title, body, base, draft state, labels and assignees, links back with "Replaces #N", and re-requests review from everyone who reviewed the old one.
Approvals don't carry over, because reviews belong to a PR. The old PR gets a "Replaced by" comment linking to the new one.
