//go:build !darwin || !cgo

package secrets

import "context"

type Keychain struct{}

func NewKeychain() Store                                     { return Keychain{} }
func (Keychain) Get(context.Context, string) (string, error) { return "", ErrUnavailable }
func (Keychain) Set(context.Context, string, string) error   { return ErrUnavailable }
func (Keychain) Delete(context.Context, string) error        { return ErrUnavailable }
