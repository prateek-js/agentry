# Skill: GitHub (existing-repo workflow)

Use when the user names a GitHub repo to work on
("fix the bug in acme/widgets", "add tests to my-org/api",
"open a PR against octocat/hello-world for the spelling issue").

## Precondition: is GitHub bound?

Check the `env` array in the `sandbox_create` response (or the
sandbox env later). If `GITHUB_TOKEN` is present, GitHub is wired:
the `gh` CLI, git over HTTPS, and every GitHub SDK that respects
`GITHUB_TOKEN` will Just Work without further setup.

If `GITHUB_TOKEN` is NOT there, **stop and tell the user**:

> GitHub isn't bound on this cluster yet. Run
> `agentry service bind github` from your CLI (you'll be prompted
> for a Personal Access Token), then ask me to retry.

Do NOT try to clone a repo without a token — the clone will work
for public repos but every push will fail with a confusing 403,
which is worse than a clean "not configured" message.

## Workflow

```
1. command_run "gh repo clone OWNER/REPO /workspace/projects/REPO"
2. command_run "cd /workspace/projects/REPO && git config user.email $GITHUB_USERNAME@users.noreply.github.com && git config user.name $GITHUB_USERNAME"
3. Detect the project kind from /workspace/projects/REPO contents:
     - package.json with "next" dep      → kind=nextjs
     - package.json (any other)          → kind=custom (read start_command from package.json scripts)
     - app.py with `import streamlit`    → kind=streamlit
     - app.py with `from fastapi import` → kind=fastapi
     - requirements.txt + no entry app   → kind=python-script
     - .html + assets only               → kind=static-html
     - else                              → kind=custom (ask user for start_command)
4. project_create name=REPO kind=<detected> — this writes
   .sandbox-project.json without touching the cloned source.
5. project_start name=REPO and iterate normally.
```

`project_create` writes the manifest into the cloned dir without
clobbering existing files — its starter-file scaffolding only fills
in files that don't already exist, so a real repo just gets the
manifest layer.

## When the user wants to ship a change

```
1. command_run "cd /workspace/projects/REPO && git checkout -b agentry/<short-slug>"
2. Make edits via file_write / file_replace / file_multi_edit as usual.
3. command_run "cd /workspace/projects/REPO && git add -A && git commit -m 'Your message'"
4. command_run "cd /workspace/projects/REPO && git push -u origin HEAD"
5. command_run "cd /workspace/projects/REPO && gh pr create --fill"
6. Echo the PR URL the user.
```

`gh pr create --fill` populates title + body from the commit
message — fast for small changes. For larger ones, build the
body with file_write into `/tmp/pr.md` and use
`gh pr create --title '...' --body-file /tmp/pr.md`.

## Anti-patterns

- ❌ Editing `git config user.email` to a real address — use the
  noreply form so the user's primary email doesn't get exposed
  in the commit history if they didn't set
  `notifications.no-reply.email` upstream.
- ❌ `git push --force` on someone else's branch — only force-push
  branches you opened.
- ❌ Committing `.sandbox-project.json` to upstream — it's an
  agentry dev convenience, not part of the project. Add it to
  `.git/info/exclude` if the upstream `.gitignore` doesn't already
  cover it.
- ❌ Cloning into `/workspace` root instead of `/workspace/projects/`
  — the project manager won't see it.
- ❌ Skipping `project_create` because the repo "already has its
  own setup" — without the manifest, `project_start` can't
  manage the lifecycle and the dashboard goes dark.

## GitHub Enterprise

If `GITHUB_API_URL` is set in the sandbox env (and isn't
`https://api.github.com`), the operator bound a GHE host. `gh` reads
it from the env automatically. No code change needed.
