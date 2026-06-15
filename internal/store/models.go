package store

import "time"

type AppStatus string

const (
	StatusStopped AppStatus = "stopped"
	StatusPulling AppStatus = "pulling"
	StatusRunning AppStatus = "running"
	StatusCrashed AppStatus = "crashed"
	StatusError   AppStatus = "error"
)

type EnvVar struct {
	Key   string
	Value string
}

type App struct {
	ID              string
	Name            string
	ImageRef        string
	Entrypoint      []string
	Command         []string
	EnvVars         []EnvVar
	WorkDir         string // container-internal working directory (image WORKDIR, e.g. "/app")
	RootfsPath      string // host path to extracted image filesystem (e.g. "/data/apps/id/rootfs")
	Status          AppStatus
	AutoStart       bool
	ExposedPort     int
	HealthEndpoint  string
	PruneAfterStart bool // delete rootfs after app starts (frees disk on ephemeral storage)
	CreatedAt       time.Time
	UpdatedAt       time.Time
	LastStarted     *time.Time
}
