package worker

import "github.com/semistrict/hotbed/internal/gitauth"

// GitAuthClient is what a worker calls on the controller to delegate GitHub
// access to its commands (gitauth.Client);
// hotbedv1connect.GitAuthServiceClient satisfies it.
type GitAuthClient = gitauth.Client

// gitAuthHelpers is what the engine needs of the worker's git
// authentication (a *gitauth.Server): the directory a chroot binds read-only
// at gitauth.ChrootDir, the socket a VM's guest reaches it by, and the
// environment pointing git and gh at it on the host and in a chroot.
type gitAuthHelpers interface {
	Dir() string
	SockPath() string
	HostEnv() []string
	ChrootEnv() []string
	Close() error
}
