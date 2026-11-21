//go:build !linux

package runtimemodule

func renameExchange(a, b string) error {
	return ErrExchangeUnsupported
}
