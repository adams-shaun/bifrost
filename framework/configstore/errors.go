package configstore

import (
	"errors"
	"fmt"
	"strings"
)

var ErrNotFound = errors.New("not found")
var ErrAlreadyExists = errors.New("already exists")

// ErrTenantNotEmpty is returned by DeleteTenant when the tenant still owns
// governance / config rows. Callers must drain the tenant (suspend + clean
// up dependents, or transfer ownership) before deletion.
var ErrTenantNotEmpty = errors.New("tenant is not empty")

// ErrReservedTenant is returned when an operation targets a reserved
// tenant ID like DefaultTenantID — the default tenant cannot be deleted
// because single-tenant OSS deployments depend on it existing.
var ErrReservedTenant = errors.New("reserved tenant")

// ErrUnresolvedKeys is returned when one or more keys could not be resolved
type ErrUnresolvedKeys struct {
	Identifiers []string
}

func (e *ErrUnresolvedKeys) Error() string {
	return fmt.Sprintf("could not resolve keys: %s", strings.Join(e.Identifiers, ", "))
}
