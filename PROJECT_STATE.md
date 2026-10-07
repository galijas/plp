# Project state

Read this first when continuing work.

## RESUME HERE (2026-10-06)

Live at https://privatelabel.dtbicom.xyz, running the latest commit
(local, GitHub, Gitea and the VPS were all at the same commit at the end
of 2026-10-06). Nothing is half-done. Open items are under "Next".

- Repo: `~/claude/Projects/PrivateLabel_Portal`
- Remotes: `origin` = git@github.com:galijas/plp.git,
  `gitea` = git@tuzla.git.bicomsystems.com:amer.g/plp.git. Push to both.
- Commits use author/committer `galiash <galijash@outlook.com>` (set via
  GIT_AUTHOR_*/GIT_COMMITTER_* env vars; no global git identity here).
- Go toolchain on the dev machine: `~/claude/.tools/go` with
  `PATH=~/claude/.tools/go/bin:$PATH GOPATH=~/claude/.tools/gopath GOCACHE=~/claude/.tools/gocache GOTOOLCHAIN=local`.
- Local test run: `bin/plportal create-admin -data-dir <dir> -username admin`,
  then `bin/plportal serve -dev-addr 127.0.0.1:8090 -data-dir <dir>`.
  UI review worked well with headless Chrome over CDP (screenshots in
  light/dark, 1440/1100/390 px).

### VPS

- `privatelabel.dtbicom.xyz` = 161.129.58.234, Ubuntu 24.04, SERVERware VPS.
- SSH as root on port 2020 (password auth; the password is given in the
  session only and is never written to any file).
- Repo on the VPS: `/root/plportal`. Saved install answers:
  `/etc/plportal/plportal.env` (domain, Let's Encrypt email
  amerg@bicomsystems.com, time zone Europe/Sarajevo, 3 GB per submission,
  firewall on, SSH port 2020).
- Deploy: push to both remotes, then on the VPS
  `cd /root/plportal && git pull && yes "" | ./install.sh`
  (Enter keeps every saved answer; the firewall rules don't change, so
  the install doesn't stop for the SSH confirmation).
- Checks: `systemctl status plportal plportal-firewall`,
  `journalctl -u plportal -n 50`, `ss -tlnp` (expect sshd on 2020,
  plportal on 80 and 443), `nft list table inet plportal_fw`.

## What exists

- Clients sign in with email + invite code (no accounts). Admins with
  username/password ("Log in as admin" on the sign-in page, or `/#admin`).
- Invite codes: tied to one email or any email; single use or unlimited;
  expiry 1 week / 2 weeks / 1 month / 3 months / none, changeable later;
  revoke/restore/delete; codes shown in plain text. Invite link
  `https://<host>/#invite=CODE&email=ADDRESS` fills in the sign-in fields;
  shown with a Copy button after creating/editing a code and on each row.
- Upload limits (2026-10-07, migration 3 applied them to the live form):
  per upload question 10 files, 10 MB each, 100 MB together; types: the
  branding list plus archives .7z .rar .tar .gz .tgz .bz2 .xz. All
  uploads of a submission go into one zip with a folder per question;
  uploaded archives sit inside it unchanged (user: unzipping twice is OK).
- Guides (2026-10-07): the four Google Drive/Docs links now point to PDFs
  served by the portal at /guides/<name> (signed-in users only, inline in
  a new tab) and /guides/all.zip ("Download All Guides" button above
  Submit). The PDFs are in the repo's guides/ (user's choice
  2026-10-07, aware the GitHub repo is public) and install.sh copies them
  to /var/lib/plportal/guides; sources in ~/claude/Resources/PLPortal
  Guides/. Migration 4 rewrote the live form's links. Other links checked:
  all third-party/downloads.bicomsystems.com; the MSDN code-signing link
  redirects to archived IE docs (told the user).
- Upload descriptions end with "Additional .svg files are preferred: ..."
  (2026-10-07, migration 5 added it to the live form).
- Form: copy of the Google Form (3 sections). Drafts auto-save; files
  upload in 16 MB chunks, resumable; required answers checked client and
  server side.
- Admin tabs: Submission Form (preview + full editor: text, description
  markup, type, required, options, upload limits, add/delete/reorder
  items and sections; each save = new form version) and Admin Panel:
  Submission Explorer (new tab), Invite Codes, Action Logs (filters,
  CSV), Accounts, Security, Configure SMTP.
- Submissions: folder `<email> - <YYYY-MM-DD HH-MM-SS>` with
  `Form Answers - ….txt` (also HTML view and PDF with bold answers) and
  `Uploaded Files - ….zip` (one folder per question). Answers are a
  snapshot of the question wording at submit time.
- SMTP notifications to every admin with an email; no answers in the
  email, only a link. Client copy (2026-10-07, on by default, checkbox in
  Configure SMTP): the submitter gets an email with the answers PDF
  attached (no uploaded files); logged as smtp.client_copy(_failed).
- Theme button "Theme: System/Dark/Light" (top right, remembered per
  browser). Header and admin tabs centered. Font Inter (self-hosted).
  Icon: document with upload arrow (from
  ~/claude/Resources/PLP_Icon_black.png, redrawn as SVG; black on light,
  white on dark; tab icon follows the portal theme).

## Decisions

- Stack: one Go binary (embedded UI, SQLite via modernc, Let's Encrypt
  via autocert), installed with `install.sh`. DT Collector is only the
  reference for how to install; UI and behavior are this project's own.
- Code uses configurable per code; full form editor; drafts; SMTP: all
  agreed with the user on 2026-10-06.
- "Pre-fill" means the invite link fills in code and email. Pre-filled
  form answers were built and then removed at the user's request
  (migration 2 drops the column).
- Host firewall: nftables only, own table `inet plportal_fw`, inbound
  SSH port(s) + 80 + 443, outbound unfiltered. No ufw: the SERVERware VPS
  kernel lacks iptables LOG/REJECT and can't load modules, so ufw fails.
  New/changed rules roll back unless a new SSH login is confirmed within
  3 minutes. (DT Collector's VPS has no host filtering: only empty ufw
  leftover tables with policy accept.)
- Time zone is not asked by the installer: Europe/Sarajevo, editable as
  PLP_TIMEZONE in the env file.
- Dark theme (changed 2026-10-07 at the user's request): DT Collector /
  SwarmDialer dark palette (#0b0d12 / #12151c / #171b25 surfaces, text
  #e7e9f0, muted #8b93a7, amber #ffb454 actions, links #5eb1ff, ok
  #3ddc97, errors #ff5470); dark sign-in button stays #50b5ff. Light
  theme unchanged (Bicom colors, below; light sign-in button #148acb).
- Visual design (2026-10-07): Bicom Systems colors from bicomsystems.com:
  light = white + light blue (#f1f7ff) with navy #0b163f text and blue
  #175cff actions; dark = navy #0b163f with white text and light blue
  #50b5ff actions; red for required and errors. Earlier: cool paper / navy ink / process cyan, magenta for
  required and errors, crop marks around sign-in and upload zones.
  Inline style attributes are blocked by the CSP; use classes.

## Next

- Configure SMTP on the live site with the real relay and send a test.
- Try a real client flow end to end over the internet, including a large
  (about 1 GB) upload.
- Mid-size screens (960 to 1340 px): the form and section overview are
  centered together, so the form sits slightly right of center. The user
  hasn't said whether to narrow the form there instead.
- Optional, offered but not requested: the same nftables firewall in DT
  Collector's installer; cleanup of the empty ufw tables on the DT
  Collector VPS.
- Daily backup covers the database only; submission files in
  /var/lib/plportal/submissions need a separate file backup.
