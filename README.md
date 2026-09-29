# RAT
Remote Agent Terminal

## Requirements

- **tmux 3.3 or later.** 3.3a (Debian 12) is the oldest version rat is tested with: older ones
  behave differently in ways rat relies on, and are not supported.
- **bash 4.4 or later**, which terminals run. macOS ships bash 3.2: install a recent one
  (`brew install bash`) and make sure it comes first in the PATH of rat.

## Your shell configuration

Terminals run bash as a login shell, which reads the bash startup files of the user running rat
(`~/.bash_profile`, `~/.profile`…). rat enforces what it relies on, but a startup file can still
defeat it: one assigning `PROMPT_COMMAND` (rather than adding to it) turns off what makes a
pasted text wait for Enter, and each of its lines then runs as soon as it is pasted. rat checks
this when it starts and warns, but does not refuse to run. A startup file changed while rat runs
is only checked at its next start.
