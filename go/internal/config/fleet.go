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

// Cherry Servers: the smallest shared server, 1 vCPU and 1 GB, Debian 13, in
// a project of our own. Paid by the month: by the hour it costs more than the
// operator's limit, so the line only goes there when the operator says so.
const (
	CherryPlan    = "B2-1-1gb-20s-shared"
	CherryImage   = "debian_13_64bit"
	CherryProject = "awg-rotator"
)

// CherryLocations are the Cherry cities used: the ones with the small server
// in stock when this was written (Stockholm and Frankfurt had none).
var CherryLocations = []string{"ams", "sia"}

// Regions are the Vultr cities the rotation picks from, by the hour. Chosen
// for low latency to Russia.
var Regions = []string{
	"sto", // Stockholm
	"waw", // Warsaw
	"fra", // Frankfurt
	"ams", // Amsterdam
}
