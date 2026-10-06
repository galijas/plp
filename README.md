# Private Label Portal

A small web portal that replaces the Google Form "Private Label Branding
Resources and Requirements". Clients fill in the form and upload their
branding files; Bicom Systems admins manage the form, invite codes and
submissions.

## How access works

- **Clients** sign in with their email and an **invite code**. No accounts.
  A code can be tied to one email or accept any email, is single use or
  unlimited, and expires after 1 week, 2 weeks, 1 month, 3 months or never.
  Admins send an **invite link** (`https://<host>/#invite=CODE&email=ADDRESS`)
  that fills in the sign-in fields. Invite codes can also pre-fill answers.
- A client session can only read the form, save its own draft, upload files
  to its own draft and submit. Uploaded files are never served back to
  clients. Drafts auto-save, so clients can come back later with the same
  code and email.
- **Admins** log in with a username and password (link on the sign-in page).
  They see two tabs: **Submission Form** (the form as clients see it, with
  Edit buttons) and **Admin Panel**:
  1. Submission Explorer (opens in a new tab)
  2. Invite Codes
  3. Action Logs (filter by time, user, action; CSV export)
  4. Accounts
  5. Security (own password and notification email)
  6. Configure SMTP (email to all admins on each new submission)

## Submissions

Each submission is a folder named `<email> - <YYYY-MM-DD HH-MM-SS>` holding:

- `Form Answers - <email> - <time>.txt`: every question followed by its answer.
  The explorer also shows it formatted and offers it as a PDF (answers in bold).
- `Uploaded Files - <email> - <time>.zip`: the uploaded files, one folder per
  question.

Submissions keep the question wording from the moment they were submitted,
so later form edits never change them.

## Install (Ubuntu 24.04)

1. On the server, clone the repository and run the installer:
   ```
   git clone https://github.com/galijas/plp.git plportal
   cd plportal
   sudo ./install.sh
   ```
   The script installs Go and the other dependencies, asks for the DNS name,
   a Let's Encrypt email and the time zone, offers a host firewall (below),
   builds the binary, sets up systemd services, gets the certificate and
   creates the first admin, printing its password once.
2. Log in at `https://<host>/#admin`, set up SMTP under Admin Panel >
   Configure SMTP, and create invite codes.

**Host firewall:** `install.sh` offers an nftables firewall that lets in only
TCP to the SSH port(s), 80 and 443 (plus ping and DHCP replies) and drops
everything else; outbound traffic is not filtered. It does not use ufw:
ufw needs iptables LOG/REJECT targets that the SERVERware VPS kernel lacks.
The SSH port defaults to what sshd listens on. Before keeping new rules, the
script asks you to confirm that a new SSH login works and rolls back after
3 minutes otherwise. Rules: `/etc/plportal/firewall.nft` (own table
`inet plportal_fw`), loaded at boot by `plportal-firewall.service`. Answer
"n" on a re-run to remove it.

**Upgrade** (press Enter at the prompts to keep the saved answers):
```
cd plportal
git pull
sudo ./install.sh
```

## Operations

| What | Where |
| --- | --- |
| Service | `systemctl status plportal`, `journalctl -u plportal` |
| Configuration | `/etc/plportal/plportal.env` (re-run `install.sh` to change) |
| Database | `/var/lib/plportal/plportal.db` |
| Submission folders | `/var/lib/plportal/submissions/` |
| Unsent uploads (drafts) | `/var/lib/plportal/staging/` (incomplete uploads are removed after 48 h) |
| Database backups | `/var/lib/plportal/backups/` (daily, last 14 kept) |
| Certificates | `/var/lib/plportal/autocert/` |

The daily backup covers the database only. Back up
`/var/lib/plportal/submissions/` with your usual file backup; it holds the
uploaded files.

Admin command line (run as the service user):

```
sudo runuser -u plportal -- plportal reset-password -data-dir /var/lib/plportal -username admin
sudo runuser -u plportal -- plportal create-admin -data-dir /var/lib/plportal -username NAME -email ADDR
```

Limits: each file question sets its own file count, size and types (the
default form allows 5 files of up to 1 GB each); all files of one submission
together are capped at 3 GB (`PLP_MAX_MB` in the env file). Uploads are
refused when the data disk would drop below 2 GB free.

## Security notes

- HTTPS only (Let's Encrypt via the built-in ACME client); port 80 only
  answers certificate challenges and redirects.
- Runs as the unprivileged `plportal` user under a hardened systemd unit.
- Optional host firewall (nftables): inbound SSH, 80 and 443 only.
- Sign-in failures are rate limited per IP (10 per 15 minutes) and logged.
  Failed client sign-ins get the same message whatever the reason, so the
  page doesn't reveal which codes or emails exist.
- Passwords are bcrypt hashed; session tokens are stored hashed.
- Strict Content Security Policy (no inline scripts or styles),
  cross-origin request protection on every form and API call.
- Downloads are always sent as attachments; uploaded files are never
  rendered by the browser.
- Notification emails contain no answers (they can include secrets such as
  the Google project secret), only a link to the submission.

## Development

```
go build -o bin/plportal ./cmd/plportal
./bin/plportal create-admin -data-dir ./dev-data -username admin
./bin/plportal serve -dev-addr 127.0.0.1:8080 -data-dir ./dev-data
go test ./...
```

Form descriptions support `**bold**`, `[link text](https://...)`, lines
starting with `- ` as lists, and blank lines between paragraphs.
