# Project state

## Status (2026-10-06)

First complete version, built and tested locally (dev mode, headless Chrome
screenshots in light/dark and phone width, curl end-to-end tests incl. a
local SMTP sink). Not yet deployed.

## Decisions

- Stack: one Go binary (embedded UI, SQLite via modernc, Let's Encrypt via
  autocert), installed with `install.sh` like DT Collector. Only the install
  approach follows DT Collector; UI and behavior are this project's own.
- Access: email + invite code for clients (no accounts); admin username and
  password. Invite codes are stored in plain text on purpose (admins must
  see them); they are random 16-character codes (80 bits).
- Code uses: configurable per code (single use / unlimited until expiry).
- Form editor: full (text, required, type, options, upload limits, add,
  delete, reorder items and sections). Each save is a new form version;
  submissions store a snapshot of the questions and answers.
- Drafts: auto-saved per (invite, email); uploads are staged per draft.
- Uploads: chunked (16 MB), resumable, offset-checked; zip is stored
  uncompressed at submit time.
- SMTP: notifies every admin with an email address; the email has no
  answers, only a link.
- Visual design: print-proof theme (cool paper, navy ink, process cyan,
  registration magenta for required/errors, crop marks around sign-in and
  upload zones). Font: Schibsted Grotesk (OFL), self-hosted.
- Host firewall: install.sh offers nftables rules (own table inet
  plportal_fw, SSH ports from sshd + 80 + 443 inbound only), no ufw
  (SERVERware VPS kernel lacks LOG/REJECT). New rules need confirmation
  from a new SSH login within 3 minutes, else they roll back.
- Theme selector: "Theme: System/Dark/Light" in the top-right, localStorage
  key `plportal-theme`.

## Next

- Create the GitHub repo and push; deploy to the VPS.
- After deploy: send a real test invite, check the certificate, SMTP with
  the real relay, and a large (1 GB) upload over the internet.
