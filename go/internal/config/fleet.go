package config

// InstanceTag marks every instance this project creates. Nothing in this tool
// ever touches an instance without it, so other servers on the same Vultr
// account are left alone.
const InstanceTag = "awg-rotator"

// Plan and OS used for every server: 1 vCPU, 1 GB, Debian 13.
const (
	Plan = "vc2-1c-1gb"
	OsID = 2625 // Debian 13 x64 (trixie)
)

// Regions the rotation picks from. Chosen for low latency to Russia.
var Regions = []string{
	"sto", // Stockholm
	"waw", // Warsaw
	"fra", // Frankfurt
	"ams", // Amsterdam
}
