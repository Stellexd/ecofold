---
name: git-workflow-standards
description: MUST READ before running git commit, creating branches, opening Pull Requests, or updating documentation.
---

# Git Workflow & Portfolio Standards

1. **Conventional Commits (English Only):**
   - Format: `<type>(<scope>): <concise summary in imperative mood>`
   - Allowed types: `feat`, `fix`, `docs`, `refactor`, `test`, `chore`, `ci`.
   - Allowed scopes: `gpu`, `detector`, `fah`, `ipc`, `daemon`, `gui`, `build`.
   - Example: `feat(gpu): implement nvml power limit controller and darwin mock`
   - git config user.name y git config user.email always for Stellexd and benjaminignacio.polanco@gmail.com
2. **Branching & Issue Linking:**
   - Never commit features directly to `main`.
   - Use feature branches tied to GitHub Milestones/Issues (e.g., `feat/m1-gpu-provider`).
   - Reference the issue in PR descriptions (`Closes #1`) and merge via **Squash and Merge** to keep `main` history clean for technical recruiters.
EOF