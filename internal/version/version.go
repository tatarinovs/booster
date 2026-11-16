package version

import (
	"embed"
	"strings"
)

//go:embed VERSION
var files embed.FS

// Number is the application version shared by all build paths.
var Number = func() string {
	data, _ := files.ReadFile("VERSION")
	return strings.TrimSpace(string(data))
}()
