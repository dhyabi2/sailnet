//go:build tools

package mobile

// Keeps golang.org/x/mobile in go.mod for gomobile bind.
import _ "golang.org/x/mobile/bind"
