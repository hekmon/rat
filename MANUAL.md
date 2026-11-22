# RAT manual

Operating RAT: running ratd as a service, its security, the file transfers, and the shell
configuration its terminals rely on. What RAT is, and how to try it, are in the
[README](README.md).

- [Deployment](#deployment): [Linux, with systemd](#linux-with-systemd) ·
  [macOS, with launchd](#macos-with-launchd) · [Upgrading](#upgrading)
- [Security](#security): [No way in for an agent to open](#no-way-in-for-an-agent-to-open) ·
  [Mutual TLS](#mutual-tls) · [A bundle of rat-tool](#a-bundle-of-rat-tool) ·
  [A sub CA of your PKI](#a-sub-ca-of-your-pki) · [Deploying safely](#deploying-safely)
- [File transfers](#file-transfers)
- [Your shell configuration](#your-shell-configuration)

## Deployment

ratd is meant to run as a service, under a Unix user of its own (see Security). Linux with systemd
is the recommended target, the only one where the kill switch is complete: stopping the service
ends every process in its control group, commands detached from their terminals included. ratd
runs on macOS too, but launchd leaves detached commands running.

### Linux, with systemd

```sh
useradd --create-home --shell /usr/sbin/nologin rat
echo 'DenyUsers rat' > /etc/ssh/sshd_config.d/rat.conf && systemctl reload sshd
install -d -m 755 /etc/rat/prod && cp -r bundle-prod/server /etc/rat/prod/
chmod 755 /etc/rat/prod/server && chown rat /etc/rat/prod/server/server.key
```

```ini
# /etc/systemd/system/ratd-prod.service
[Unit]
Description=RAT: persistent terminals for agents, tenant prod
# the address of --listen must be up when ratd binds it, or ratd exits: a VPN address needs
# the unit of the VPN before this one as well (After=wg-quick@wg0.service)
Wants=network-online.target
After=network-online.target

[Service]
# a Unix user for RAT alone: what agents do is bounded by it
User=rat
# ratd tells systemd when it serves: systemctl start waits for it, and reports a failed startup
Type=notify
ExecStart=/usr/local/bin/ratd --bundle /etc/rat/prod/server --listen :7281
# the default, and what the kill switch relies on: stopping the service kills every process
# left in its control group, detached commands (nohup, setsid, daemons) included
KillMode=control-group
# above the worst stop of ratd: 10 seconds for the calls in flight, then 7 for its terminals
TimeoutStopSec=30
# what the service may take of the machine, every process counted, terminals included: values to
# fit the machine and the work of the agents (see Resources below)
#TasksMax=2048
#MemoryMax=8G
#CPUQuota=400%

[Install]
WantedBy=multi-user.target
```

```sh
systemctl enable --now ratd-prod
journalctl -u ratd-prod -f
```

- **The only door is ratd's.** `useradd` gives the account no password (nothing matches the locked
  field it leaves: sshd refuses an empty password and a guess alike, password authentication on
  or not) and, with `--shell`, no login shell, which the terminals do not need: they run the bash
  ratd finds in its PATH, whatever the shell of the account. What closes ssh is `DenyUsers`: an
  agent can add a key to `~/.ssh/authorized_keys`, which a login shell turns into a shell, and a
  `nologin` one still into a tunnel (`ssh -N -L` needs no shell). `DenyUsers` refuses the account
  before it authenticates, key, password and tunnel alike (measured with OpenSSH 9.2 on Debian 12;
  leaving it out of an `AllowUsers` list does the same). Check with `sshd -T | grep -i denyusers`.
  Reach the account with `sudo -u rat`, as the attach command does (see Security): it runs the
  command itself, where `su -` and `sudo -i` run the login shell, and are refused.
- **The bundle belongs to root, but for its key.** ratd only reads it, and the agents run as its
  user: a bundle that user could write, an agent could replace with one of its own, whose clients
  ratd would let in at its next start (measured on Debian 12): ratd warns at startup of a bundle
  its user can write, naming the paths. The key stays the user's, ratd having to read it and
  refusing it readable by others: an agent can spoil it, and ratd then refuses to start, or copy
  it, which stopping the service does not revoke (see Security).
- **The kill switch.** `systemctl stop ratd-prod` stops ratd, its endpoint first, the terminals
  and the commands they run, and whatever else is left in the service: commands detached from
  their terminals included (measured with systemd 252: a `nohup` command started by an agent is
  gone; with systemd 255 and tmux 3.4 too, whose panes ratd keeps in the service, see
  [`tmux/README.md`](tmux/README.md)). Keep `KillMode=control-group`: started by hand, ratd can
  not stop what detached from its terminals.
- **No `Restart=`, on purpose.** ratd restarts its terminals when they die, and only exits when
  it can not keep them up: the service then shows as failed, for whoever monitors it.
- **Resources are bounded by the service, not by ratd.** Whoever calls ratd has a shell: a loop
  of `tmux new-window`, a fork bomb or a build eating the memory gets around any count ratd could
  keep of windows or calls. The control group of the service counts every process it holds,
  terminals and detached commands included. `TasksMax=` caps its processes and threads (the
  default of systemd, 15% of the limit of the system, usually allows thousands), `MemoryMax=`
  keeps the out-of-memory killer within the service, which kills its largest process (a build,
  rather than another service of the machine), `CPUQuota=` keeps the machine responsive. Their
  values depend on the machine and the work of the agents: the unit holds them commented. At a
  limit, starting a process fails: windows and commands do not start, tool calls fail, and the
  stop still ends everything, systemd killing what is left in the group. A file operation stuck
  on a hung file system holds its content in ratd until it returns, the agent being told to check
  the file before writing again: `MemoryMax=` also bounds what retries could pile up. Rejected:
  limits in ratd (windows per session, calls at once), which a single command typed in a
  terminal gets around.
- **One unit per tenant**, each with its bundle and port. Tenants whose agents must not reach each
  other need a Unix user each (see [Users, tenants and
  sessions](README.md#users-tenants-and-sessions)).
- **Logs** go to the journal: every tool call (the client, the tool, the outcome, never the
  content of what agents type or read), the start and stop, the bundle expiry warnings. Each
  level is a priority of the journal: `journalctl -u ratd-prod -p warning` shows what needs an
  admin.

### macOS, with launchd

Create a standard user named `rat` (System Settings, Users & Groups; not an administrator), and
install the bundle as on Linux. Unlike `useradd`, this gives the user a password and a login
shell: if Remote Login is on, leave `rat` out of the users it allows (General, Sharing, Remote
Login), and take its login shell away, which the terminals do not need:
`sudo dscl . -create /Users/rat UserShell /usr/bin/false`. Then, as a daemon of the system
running as that user, in `/Library/LaunchDaemons/com.github.hekmon.rat.prod.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.github.hekmon.rat.prod</string>
  <key>UserName</key>
  <string>rat</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/ratd</string>
    <string>--bundle</string>
    <string>/etc/rat/prod/server</string>
    <string>--listen</string>
    <string>:7281</string>
  </array>
  <!-- launchd starts jobs with /usr/bin:/bin:/usr/sbin:/sbin, whose bash (3.2) ratd refuses:
       the one of Homebrew must come first. HOME is where terminals start. -->
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    <key>HOME</key>
    <string>/Users/rat</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>StandardErrorPath</key>
  <string>/Library/Logs/rat/ratd-prod.log</string>
</dict>
</plist>
```

```sh
sudo install -d -o root -g wheel -m 755 /Library/Logs/rat
sudo touch /Library/Logs/rat/ratd-prod.log
sudo chown rat /Library/Logs/rat/ratd-prod.log
sudo chflags sappnd /Library/Logs/rat/ratd-prod.log
sudo chown root:wheel /Library/LaunchDaemons/com.github.hekmon.rat.prod.plist
sudo launchctl bootstrap system /Library/LaunchDaemons/com.github.hekmon.rat.prod.plist
tail -f /Library/Logs/rat/ratd-prod.log
```

- **The kill switch is partial.** `sudo launchctl bootout system/com.github.hekmon.rat.prod`
  stops ratd, its endpoint first, the terminals and the commands they run in the foreground. But
  launchd only ends the process group of the service, not what it started: commands detached from
  their terminals (`nohup`, `setsid`, daemons) outlive it (measured: a `nohup` command started by
  an agent is left running). macOS has nothing like the control groups of systemd. With a user
  for rat alone, end them all after the service: `sudo pkill -KILL -u rat`.
- **The log is out of the agents' reach**, as the journal is on Linux. launchd opens it as the
  user of the job, `rat`, appending to it: the file has to be that user's, or the job does not
  start. In a directory of root, with the system append-only flag (`sappnd`), which only root
  clears, agents can add lines to it, but neither rewrite, truncate, rename nor remove it. To
  rotate it, stop the service (ratd keeps writing to the file it opened), clear the flag (`sudo
  chflags nosappnd`), move the file, and create it again as above. Taken from the source of launchd
  (`launchd-842`, the last one published: the user is taken before the file is opened, with
  `O_APPEND`) and from measures of others on recent macOS, not measured with RAT.
- **No `KeepAlive`, on purpose**, as there is no `Restart=` on Linux.
- **One plist per tenant**, each with its label, bundle and port.

### Upgrading

Upgrading ratd costs every terminal: the terminals belong to the service and stop with it, which
is what makes stopping it a kill switch. The windows, their history and the commands running in
them are lost, with systemd the commands detached from their terminals as well. Files stay, and so
does what agents set up outside the service (see What outlives the service). Plan it as you would
the reboot of a machine people work on:

```sh
systemctl status ratd-prod
sudo -u rat tmux -L rat-prod list-windows -a -F '#{session_name} #{window_name}: #{pane_current_command}'
install -m 755 ratd /usr/local/bin/ratd
systemctl restart ratd-prod
```

- **Who would be cut.** The status tells when each client last called, and the calls waiting for a
  command. `list-windows` tells what runs in the windows of each client, `bash` being a terminal at
  its prompt, or running a script.
- **The binary is replaced while ratd runs.** `install` puts a new file in its place, and the
  running ratd keeps the old one until it stops. `cp` onto a running binary fails (`Text file busy`,
  measured on Debian 12).
- **Agents find out by themselves.** While ratd restarts, rat answers each call that ratd is
  unreachable, and keeps running: harnesses need no restart. The next call tells the agent its
  terminals did not exist, and finds a fresh `main`. What ran has to be started again.
- **rat and rat-tool go with ratd.** They are released together: install them on the machines of
  the harnesses as well, where a harness runs the new rat when it next starts it. A rat of another
  version keeps relaying, but only the same version is sure to agree with ratd on its tools.
- **File transfers take both.** A rat offering them needs a ratd serving them: with an older ratd,
  rat tells the agent that ratd must be upgraded, and an older rat offers no transfers at all.
  Upgrading ratd cuts the transfers under way, as it cuts the terminals.
- **On macOS**, `launchctl bootout` then `bootstrap` take the place of the restart, and commands
  detached from their terminals outlive it (see macOS, with launchd).
- **Renewing a certificate takes no restart** (see Renewal).

## Security

ratd hands out a shell: who gets in is decided by mutual TLS, and what they can do once in by the
Unix user it runs as. Stopping ratd leaves no terminal and no way in; run as a service with
systemd, it also stops what agents detached from their terminals (on macOS, see Deployment).

### No way in for an agent to open

An agent has a shell on the machine: it must not be able to hand a way in to anyone, another
agent included, through rat. With a deployment following this manual, it can not:

| An agent trying to | fails, as |
|---|---|
| issue itself a client certificate | no CA key is on the machine: the one of a bundle was never written, the one of a sub CA stays in your PKI |
| use the server key and certificate as a client | ratd requires client authentication, which the server certificate does not carry (see Mutual TLS) |
| put a CA of its own in place | the bundle is root's, ratd warning at startup when its user can write it, and a running ratd takes no new CA before a restart (see Renewal) |
| un-revoke a certificate | a CRL is signed by the CA, and one older than the CRL held is refused (see Revocation) |
| add an ssh key to the account | sshd refuses the user (`DenyUsers`, see Linux, with systemd) |
| reach another session through ratd | the client certificate names the session, which no request chooses |

These hold on conditions, each told in this manual, which a deployment breaking one of them
loses:

- **ratd runs as a user of its own, never root** (see Avoid root): root reads and writes
  everything, the bundle and the configuration of sshd included.
- **No sudo beyond named commands** (see No sudo, or sudo for named commands only): `sudo` for
  every command is root.
- **The bundle is generated off the server**, its server directory alone copied there: generating
  it on the server writes the keys of every client there, each a way in for an agent reading it.
  Nor is the key of a sub CA ever on the server.
- **The bundle belongs to root, but for its key** (see Linux, with systemd).
- **sshd refuses the user** (`DenyUsers`).

What rat can not prevent is an agent opening a way in as its Unix user, without rat:

- **A listener or a tunnel** (`ncat -l`, `ssh -R` to a machine outside): a firewall, inbound and
  outbound, bounds it (see Choose who can reach the port).
- **Its own ratd**, from a bundle it generates, on another port: the firewall again.
- **The terminals of the other sessions and tenants of its Unix user**, through their tmux
  sockets (see [Users, tenants and sessions](README.md#users-tenants-and-sessions)): a Unix user,
  or a machine, per tenant whose agents must not reach each other.
- **What outlives the service** (crontab, user services): a Unix user of its own lets you find it
  and lock it (see What outlives the service).

Stopping the service stops all of it but what belongs to another tenant (its own service), and
the last, with systemd (see The kill switch).

### Mutual TLS

TLS is what HTTPS uses: the connection is encrypted, and the client checks the certificate of the
server, to know it talks to the right one. With mutual TLS, the server checks a certificate of the
client as well: ratd only talks to clients presenting a certificate issued by the CA of its
tenant. Any other connection is cut during the handshake, before a single request reaches ratd:
there is no login page, no password to guess, nothing to try.

It protects as SSH keys do, and in the same way: each side holds a private key that never leaves
it, and proves it holds it at each connection. A client key is to ratd what an SSH private key is
to a server; the CA of the tenant stands for `authorized_keys`, the certificates it issued being
the keys listed:

- **Nothing travels that would let a client in.** A password or an API key would travel with
  every request and, kept in clear, sit next to the terminals, readable by the agents running
  there. ratd holds its own key and the certificate of the CA, which is public.
- **One role per certificate.** Each certificate carries one extended key usage, server or client
  authentication, which each side requires of its peer: the certificate of ratd does not stand
  for a client (`openssl x509 -in server.crt -noout -text` shows it).
- **Modern and fixed:** TLS 1.3 only, with keys that change at each connection (what is captured
  today can not be decrypted with a key stolen tomorrow).
- **On every address, loopback included**: other users of the machine reach loopback too.

Unlike HTTPS on the web, the client does not check the name of the host it reaches: the CA is the
tenant's alone, and a certificate it issued for server authentication proves being its ratd,
wherever it is reached (a tunnel, another address). Most harnesses can not accept this, nor
present a certificate: the rat stdio bridge does it for them. And unlike SSH, no session is tied
to a connection: the terminals keep running whether agents are connected or not, until the
service stops.

The CA of a tenant is a bundle that rat-tool generates, standing alone, or a sub CA of your PKI.

### A bundle of rat-tool

A bundle is closed: its CA signed the certificates of the server and of the clients, generated
together, and its key was never written.

- **Nobody can issue a certificate afterwards**, not even someone controlling the server: nothing
  on the server lets a client in.
- **Revoking is generating a new bundle**, and deploying it: no certificate can be added to it
  nor removed from it, adding a client included. This holds for the server key too, which the
  agents can read (see Deploying safely).
- **ECDSA P-256 certificates, valid 10 years**: every side warns once less than a year remains.
- **Nothing beside rat**: no PKI to run, no CRL, nothing fetched.

### A sub CA of your PKI

When your organization runs a PKI, a sub CA of it can take the place of the CA of a bundle: its
certificates fill the same directories, ratd and rat check them the same way, and your PKI issues
them. A client is added by issuing its certificate, with nothing to deploy to ratd or to the other
clients; certificates are revoked with a CRL, and renewed without a restart.

The guarantee of a closed bundle goes: whoever can use the key of the sub CA, in your PKI, can
issue themselves a shell as the user running ratd. And the sub CA is rat's alone, one per tenant,
never your root nor an issuing CA shared with other uses: ratd lets in every certificate it signs
for client authentication, and clients take for ratd every certificate it signs for server
authentication.

#### What rat checks

Its certificates follow the rules of a bundle, which `rat-tool bundle inspect` checks as ratd and
rat do:

- **The sub CA signs no CA:** its path length is limited to 0 (`pathlen:0`). Otherwise ratd would
  let in the certificates of every CA under it.
- **Names:** the sub CA and the server certificate carry the tenant as their common name
  (`CN=prod`), each client certificate its session (`CN=alice`), as plain names: letters,
  digits, `_` and `-`, up to 32 characters, a session not named as tmux names its clients
  (`client-` and digits, `tty…`, `console`). The rest of the subject is yours. ratd refuses a
  client of another name at each of its requests (403).
- **One role per certificate:** server authentication for ratd, client authentication for a
  client, never both (extended key usage).
- **No address needed:** the server certificate may carry DNS names or IP addresses, clients do
  not check them.
- **A CRL distribution point over http or https**, in every certificate or in none, the sub CA
  allowed to sign CRLs (`cRLSign`, see Revocation).

As openssl extension sections, for the sub CA of tenant `prod` and its two templates:

```ini
# the sub CA of tenant prod: CN=prod, the rest of its subject yours
[ rat_ca ]
basicConstraints = critical, CA:true, pathlen:0
keyUsage         = critical, keyCertSign, cRLSign

# the server certificate of ratd: CN=prod
[ rat_server ]
basicConstraints      = critical, CA:false
keyUsage              = critical, digitalSignature
extendedKeyUsage      = serverAuth
crlDistributionPoints = URI:http://pki.example.com/rat-prod.crl

# a client certificate: CN=alice, its session
[ rat_client ]
basicConstraints      = critical, CA:false
keyUsage              = critical, digitalSignature
extendedKeyUsage      = clientAuth
crlDistributionPoints = URI:http://pki.example.com/rat-prod.crl
```

#### Files

Each directory holds the files of a bundle: `ca.crt`, the sub CA alone (not the chain up to your
root, as every certificate of the file would be trusted), and the certificate and key of its
side, ECDSA or RSA, the key unencrypted in PKCS#8 and readable by its owner only. From a key in
another format, or from a `.pfx` (what AD CS exports), here for alice:

```sh
openssl pkcs8 -topk8 -nocrypt -in alice.key.pem -out client.key
openssl pkcs12 -in alice.pfx -clcerts -nokeys -out client.crt
openssl pkcs12 -in alice.pfx -nocerts -nodes | openssl pkcs8 -topk8 -nocrypt -out client.key
chmod 600 client.key
```

#### Revocation

ratd and rat fetch the CRL of the sub CA from the CRL distribution point of their own
certificate, the first over https, else the first over http, whatever their order: AD CS lists
an LDAP one first, add one over http or https. A certificate naming none revokes nothing: name
it in every template of the sub CA, as a side whose certificate names none refuses a peer whose
certificate names one, the CA revoking what it can not check. `rat-tool bundle inspect` tells,
offline, where each directory fetches its CRL from, or that it names none.

ratd fetches it while starting, and refuses to start without a valid one; rat at its first
request. Both fetch it again as they are used, every 15 minutes in the background, a minute after
a fetch failing, and at once when the CRL they hold is stale.

- **Reachable from every side.** The CRL is fetched by ratd, and by rat on every machine running a
  harness, through the proxy of their environment if any (`HTTPS_PROXY`, `HTTP_PROXY`): a laptop
  off the VPN refuses every request once the CRL it holds goes stale.
- **Over http, a CRL can be replayed.** It is signed, so nobody can forge one, but a side holding
  none yet (ratd starting, rat at its first request) takes any CRL of your sub CA still valid:
  someone on its network can hand it an older one, un-revoking what was revoked since, until
  that CRL's next update. A running side refuses a CRL older than the one it holds. A
  distribution point over https, or CRLs valid for a short time, bound it.
- **Failing closed.** A side refuses everything while it can have no valid CRL: an outage of your
  PKI longer than the validity of its CRL cuts every agent. A fetch failing is a warning in the
  logs, and in the status of ratd, with when the CRL held goes stale: CRLs valid for days, and
  published daily, leave time to act.
- **Revoking takes up to 15 minutes**, once your PKI publishes the CRL: a side checks every request
  against the CRL it holds, and fetches a newer one 15 minutes after the last fetch, from the next
  request on, which the CRL held still answers. Nothing makes ratd fetch it sooner but a restart,
  which stops every terminal: to cut a client at once, stop the service, and start it once your
  PKI published the CRL, which ratd fetches while starting.
- **Revoking a client** cuts it at its first request once ratd holds the CRL: the answer tells the
  client why. Its terminals keep running: stop them, or the service, as needed.
- **Revoking the server certificate** makes every client refuse that ratd, once each holds the
  CRL: a server key copied off the machine is revoked with its certificate. Issue the server a new
  certificate, under the same sub CA: the clients keep theirs.

#### Renewal

Certificates of your PKI, valid a year or less, warn in the last tenth of their validity,
telling what to issue again, and stop at their expiry, connections kept alive included: ratd
refuses an expired client at its next request, and rat an expired ratd. Renewing a certificate
takes no restart: replace the files of its directory, and ratd and rat use them from their next
connection on, the terminals and the calls in flight untouched. The renewed certificate must be
of the same sub CA and name, its key new or not: a new key is a warning in their logs, and in
the status of ratd for a day, for you to see a key you did not rotate. Files refused (another
CA, another name, a check failing) are a warning, the certificate in use kept; one not valid
yet, a clock behind, is taken once valid. A new sub CA takes a restart of ratd and of the
harnesses, and is to be deployed to every client.

Write the files of a renewal next to the old ones, then rename them in place (`mv`): the key
owned by the user of ratd, mode 600, as ratd refuses a key readable by others, the certificates
root's (see Deployment). The key and the certificate renamed one after the other may be seen
apart: refused, then taken at the next change, a warning and nothing broken.

### Deploying safely

RAT is not a sandbox: terminals run as the user running ratd, and an agent can do whatever that
user can. Tenants and sessions keep agents apart from mistakes, not from a malicious agent. RAT
enforces what it can (mutual TLS, bounded reads, terminals free of your configuration), and
leaves the rest to how you deploy it:

- **Avoid root.** ratd as root hands root to every agent, and to whoever holds a client key: one
  mistaken command, or an instruction an agent read in a web page or a file, and the machine is
  lost. Unless the machine is meant to be the agent's (a machine you can lose, given to it whole),
  run ratd as an unprivileged user (see Deployment). ratd warns at startup when run as root: a
  service missing its `User=` runs silently as root otherwise.
- **A Unix user for RAT, and for nothing else.** What agents do is bounded by what this user can
  do: give it none of your own files, no password, and no login but ratd (no login shell, and
  sshd refusing it: see Deployment).
- **No sudo, or sudo for named commands only.** When agents must run privileged commands, grant
  the user these commands only, never every command (`ALL`), in a file of `/etc/sudoers.d` edited
  with `visudo -f`, which refuses a file with an error:

  ```
  rat ALL=(root) NOPASSWD: /usr/bin/systemctl restart myapp, /usr/bin/journalctl --no-pager -u myapp
  ```

  - Full paths, and the arguments in full. A wildcard (`*`) matches any arguments, spaces and
    options included: `systemctl restart *` restarts any service, and does more.
  - No command that can start another one or write any file: editors (`vim`, `nano`), `find`,
    `tar`, `cp`, `tee`, shells and interpreters, scripts the user can change. Each is root for
    whoever runs it.
  - No pager: `journalctl` or `systemctl status` open `less` under sudo (which resets the
    environment, `PAGER` included), whose `!` runs a shell as root. Grant them with
    `--no-pager`.
  - `NOPASSWD` is needed, agents typing no password, and acceptable because the list is closed.
    Check what it grants with `sudo -l -U rat`.
- **No secrets in its reach.** Agents read what the user reads: keep credentials, tokens and keys
  out of its home and environment. Keys it needs to reach other machines should be of their own,
  restricted on the other side (`authorized_keys` options, a restricted command).
- **Client keys are credentials.** A client key grants a shell as that user: give each harness its
  own (logs then tell who did what), never copy one elsewhere, and generate a new bundle to revoke
  one (bundles are closed: no certificate can be added nor removed), or revoke its certificate in
  your PKI, which takes up to 15 minutes (see Revocation). A leaked client key is an incident: stop
  the service to cut it at once, as revoking does not. Whoever used it had that shell, and could
  copy the server key (see below) or leave something behind. The logs tell
  whether it was used, each call naming its client and the address it came from. Then revoking the
  client alone would not be enough: generate a new bundle and deploy it to ratd and every client
  (with a sub CA, revoke the server certificate as well, and issue a new one), and look for what
  the user may have kept.
- **The server key is readable by the agents.** ratd reads it as its user, the one the terminals
  run as: an agent can copy it off the machine. It lets no one into ratd: its certificate is only
  valid for a server, and ratd only accepts certificates valid for a client (see Mutual TLS). So
  its holder can not be a man in the middle, relaying the calls of clients to ratd: only pose as
  ratd to them, wherever they reach for it, on their way to the machine or on its port once ratd
  is stopped. Their agents then read screens made up to instruct them, and what they send is
  collected. Stopping the service does not revoke the key: after an incident, or any doubt about
  what ran as that user, generate a new bundle and deploy it, to ratd and every client, or, with
  a sub CA of your PKI, revoke the server certificate and issue a new one.
- **Choose who can reach the port.** `--listen :7281`, the default, answers on every address of
  the machine, the internet included when the machine is on it, and mutual TLS is made for
  that: whoever holds no client certificate is refused during the handshake. What stays exposed
  is the handshake itself, to a flaw yet unknown in Go's TLS or in ratd. A firewall letting only
  the machines of the agents in removes most of that exposure at no cost, and a private or VPN
  address (`--listen 10.8.0.2:7281`) all of it.
- **Watch, read-only.** Attach to the terminals of a tenant as its user, in read-only mode:
  `sudo -u rat tmux -L rat-prod attach -r -t alice` (the socket is named after the tenant, here
  `prod`, and the session after the client, here `alice`: the terminals of that client). Move
  between its windows with `C-b n`, `C-b p`, `C-b l` and `C-b 0` to `C-b 9`; read-only refuses
  the window list (`C-b w`), and scrolling back (`C-b [`) before tmux 3.7. The terminals are the
  agents': read-only, what you type does not reach them (tmux refuses every key but the window
  switches rat binds), and a window you resize stays so.
- **Keep a trail.** The logs of ratd tell who called, from where, when, which tool on which
  window, never what was typed or read (see Logs in `cmd/ratd/README.md`): the transcripts of the
  harnesses hold that. The audit of the system records every process the user runs, with its
  arguments, whatever started it (scripts and detached commands included), out of the reach of
  agents. With `auditd`, in `/etc/audit/rules.d/rat.rules`:

  ```
  -a always,exit -F arch=b64 -S execve -F uid=rat -k rat
  -a always,exit -F arch=b32 -S execve -F uid=rat -k rat
  ```

  Then `ausearch -k rat -i` lists them. A command run through `sudo` runs as root: sudo logs it
  itself.
- **What outlives the service.** Stopping the service stops the terminals and whatever agents
  started, detached or not (with systemd; on macOS, end what is left with `sudo pkill -KILL -u
  rat`, which reaches every tenant of the user, see Deployment). What an agent set up to outlive
  it, as the user (crontab, user services, `~/.ssh/authorized_keys`, files), a dedicated user lets
  you find all at once, and cut by locking the account: `usermod --expiredate 1 rat`, which expires
  the account itself, not only its password (`passwd -l`, which stops neither cron jobs nor ssh
  keys): cron then runs none of its jobs, and sshd refuses it even without `DenyUsers` (measured
  with Debian 12).

## File transfers

With `--transfers-dir DIR`, rat offers agents `upload_file` and `download_file`: they copy files by
path between DIR, on the machine of the harness, and the machine of ratd, of any size, binary
included, their content never entering the context of the model. Without it, rat offers no
transfer. In the harness, with its client directory:

```json
"args": ["--server", "host:7281", "--bundle", "/home/you/.config/rat/prod/alice",
         "--transfers-dir", "/home/you/rat-transfers/prod"]
```

- **A directory of their own.** Agents read and replace any file of that directory, through rat,
  as the user running the harness, and nothing outside it: their paths are relative to it, and
  `..` or a link leading out are refused. Choose one for that use alone, created beforehand (rat
  refuses to start without it): never your home, a project (a new `.git/hooks/pre-commit` is code
  execution), nor a directory a program runs or loads files from. Replacing a file takes the
  `overwrite` parameter, which guards against mistakes, not against an agent misled by what it
  read: the directory is the boundary.
- **On the machine of ratd**, transfers read and write wherever its user can, as `write_file` and
  `read_file` do: the Unix user is the boundary there (see Deploying safely).
- **All or nothing.** A transfer writes a temporary file next to its destination
  (`.NAME.rat-XXXXXXXX`), put in its place once complete: a transfer cut, refused or canceled
  leaves the previous version whole. A file still being written (a log, a build output) fails the
  transfer rather than being copied torn. A symbolic link at the end of a path is refused, naming
  where it leads. Neither side syncs to disk: after a power cut, a file just copied may be lost, as
  any file just written; run `sync` where it matters. A rat or a ratd killed during a transfer
  (`SIGKILL`, a crash) leaves its temporary file behind, which can be removed.
- **Permissions, as `scp` copies them.** A new file gets the permission bits of its source, minus
  the umask; a replaced file keeps its own, the agent told when they differ from those of the
  source. A replaced file is a new file in its place: it belongs to the user that wrote it, and its
  hard links and extended attributes are lost.
- **Long transfers.** A call waits 50 seconds at most; a longer copy goes on in rat, the agent told
  its progress, and finds it again by calling with the same paths. Stopping rat (the harness
  quitting) or ratd cuts the transfers under way, removing their temporary files: only an upload
  ratd already received whole may still be written.
- **Through the same door.** Transfers reach the port of ratd with the same client certificate,
  through the proxy of the environment if any: nothing else to open. Both sides log one line per
  transfer (the paths and the size, never the content), and `systemctl status` counts the
  transfers in flight.

## Your shell configuration

Terminals run bash as a login shell, which reads the bash startup files of the user running RAT
(`~/.bash_profile`, `~/.profile`…). With a user for RAT alone (see Deployment), these startup
files are RAT's: leave them as the account was created. What makes a shell pleasant to a human has
no use for agents, and gets in their way: a prompt framework such as starship hides how commands
exited, and an alias such as `rm='rm -i'` makes a command ask a question the agent did not expect.
Running RAT as your own user brings your configuration to the terminals of the agents.

RAT enforces what it relies on, but a startup file can still defeat it: one assigning
`PROMPT_COMMAND` (rather than adding to it) removes what makes a pasted text wait for Enter, and
unless bash does it by itself (5.1 and later, without an inputrc turning it off), each line of a
pasted text then runs as soon as it is pasted. It also removes what tells RAT when a command
finished, and how. A command run before RAT's does less: one changing the exit status hides the
status (starship does, `history -a` added in front does), and any, even one keeping it (direnv),
hides the status of each command of a pipeline: `command | tee log` then tells the status of tee
only. RAT checks what the bash prompt ends up with when it starts, warns, and only tells agents
what holds, but does not refuse to run. A startup file changed while RAT runs is only checked at
its next start: meanwhile, the windows created since tell agents they record no prompt, why, and
how to fix it.
