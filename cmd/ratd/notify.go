package main

import (
	"net"
	"os"
)

// notifySystemd tells the service manager the state of ratd (sd_notify(3), READY=1 or STOPPING=1)
// when it asked for it by naming its socket in NOTIFY_SOCKET (Type=notify), and does nothing
// otherwise: started by hand, or by launchd. A name starting with @ is an abstract socket, which
// net handles. The variable is left set, terminals included: systemd only listens to ratd
// (NotifyAccess=main, the default of Type=notify).
func notifySystemd(state string) error {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return nil
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(state))
	return err
}
