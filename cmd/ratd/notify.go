package main

import (
	"net"
	"os"
)

// notifier tells the service manager the state of ratd (sd_notify(3)): READY=1, STOPPING=1 and
// STATUS=. Its zero value tells nothing: ratd started by hand, by launchd, or in tests.
type notifier struct {
	// socket is the socket systemd named in NOTIFY_SOCKET (Type=notify); a name starting with @
	// is an abstract socket, which net handles
	socket string
}

// takeNotifySocket returns the notifier of the socket systemd named in NOTIFY_SOCKET, and removes
// the variable from the environment, before anything inherits it, as sd_notify does when asked
// (unset_environment). The terminals inherit ratd's environment, and systemd's own tools report
// how they exit to that socket (EXIT_STATUS=, ERRNO=, measured with systemd 255): each systemctl
// or journalctl run in a terminal, or systemd-detect-virt in a login MOTD, would reach systemd,
// which drops what is not from ratd (NotifyAccess=main, the default of Type=notify) with a
// warning in ratd's journal each time.
func takeNotifySocket() notifier {
	socket := os.Getenv("NOTIFY_SOCKET")
	_ = os.Unsetenv("NOTIFY_SOCKET")
	return notifier{socket: socket}
}

// notify sends state to systemd, if it asked for it.
func (n notifier) notify(state string) error {
	if n.socket == "" {
		return nil
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: n.socket, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(state))
	return err
}
