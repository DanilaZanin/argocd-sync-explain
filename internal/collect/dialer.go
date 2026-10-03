package collect

import "k8s.io/apimachinery/pkg/util/httpstream"

type fallbackDialer struct{ primary, secondary httpstream.Dialer }

func fallback(primary, secondary httpstream.Dialer) httpstream.Dialer {
	return &fallbackDialer{primary, secondary}
}

func (d *fallbackDialer) Dial(protocols ...string) (httpstream.Connection, string, error) {
	conn, proto, err := d.primary.Dial(protocols...)
	if err != nil {
		return d.secondary.Dial(protocols...)
	}
	return conn, proto, nil
}
