package proto

// Names of the files inside the gist. They are deliberately single letters so
// the gist gives nothing away to anyone who stumbles on it.
//
// Keeping them separate means the super client writing a command never
// disturbs the configuration the apps are reading.
const (
	FileConfig     = "c" // current configuration, written by the server
	FileStatus     = "s" // server's report about itself
	FileCommand    = "q" // instruction from the super client
	FileRevocation = "r" // revocation record from the super client
	FileRelease    = "u" // announcement of a new version of the apps, from the operator
)

// AllFiles lists every file the gist is created with.
var AllFiles = []string{FileConfig, FileStatus, FileCommand, FileRevocation}
