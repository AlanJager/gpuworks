//go:build !linux

package securefile

import "os"

func removeAccessACL(*os.File) error { return nil }

// CheckDirectoryACL checks Linux POSIX ACLs; other platforms use the private
// owner policy selected by the installation's service context.
func CheckDirectoryACL(*os.File) error { return nil }

// CheckFileACL inspects Linux POSIX ACL metadata without opening contents.
func CheckFileACL(string) error { return nil }
