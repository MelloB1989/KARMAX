package sandbox

import "fmt"

// Open returns the driver named. Unimplemented drivers say so explicitly —
// silently falling back to a different one would run the sandbox somewhere
// the operator didn't ask for.
func Open(name string) (Driver, error) {
	switch name {
	case "docker":
		return NewDockerDriver()
	case "ecs":
		return NewECSDriver()
	case "k8s":
		return nil, fmt.Errorf("sandbox: driver %q is not implemented yet", name)
	default:
		return nil, fmt.Errorf("sandbox: unknown driver %q", name)
	}
}
