// Package connect is how a client reaches ratd: the address and path ratd serves, and the flags
// naming the ratd to reach and the client directory to present to it.
//
// The flags are declared once, here, for rat and rat-tool check: a harness command line is checked
// by copying it, with the same names and the same checks. They take HOST:PORT rather than a URL:
// the scheme is always https and the path always [Path], less to get wrong. The server has no
// default, as which machine to reach is the whole point of the configuration. ratd serves [Path],
// and listens on [DefaultPort] unless told otherwise: both sides of the contract are declared here.
//
// It lives under ratd, which defines how it is reached, and is not internal, for rat and rat-tool
// to import it (Go keeps a package internal to ratd from them). Package mtls, whose contract goes
// both ways and is core to rat, stays at the root of the module, where it is audited first.
package connect
