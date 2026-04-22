package oauth2

import "github.com/koutaku/koutaku/core/schemas"

var logger schemas.Logger

func SetLogger(l schemas.Logger) {
	logger = l
}
