// Build a custom rclone binary that includes the Autodesk Fusion Data backend.
package main

import (
	_ "github.com/rclone/rclone/backend/all"
	"github.com/rclone/rclone/cmd"
	_ "github.com/rclone/rclone/cmd/all"
	_ "github.com/rclone/rclone/lib/plugin"

	_ "github.com/schneik80/rclone-fusiondata/backend/fusiondata"
)

func main() {
	cmd.Main()
}
