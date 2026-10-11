package probe

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hstern/go-activesync/eas"
)

// The status codes of ActiveSync are not one table: the same number means
// different things for different commands (FolderSync 9 is "synchronization
// key mismatch", Sync 9 a disk-space problem). The EAS library names every
// status with one shared table, so its error texts can be wrong; StatusName
// uses the tables of MS-ASCMD per command instead. Codes from 101 up are common
// to all commands (MS-ASCMD 2.2.2).

var commonStatus = map[int]string{
	108: "DeviceIdMissingOrInvalid",
	109: "DeviceTypeMissingOrInvalid",
	110: "ServerError",
	111: "ServerErrorRetryLater",
	113: "MailboxQuotaExceeded",
	114: "MailboxServerOffline",
	115: "SendQuotaExceeded",
	116: "MessagePreviouslySent",
	117: "MessageHasNoRecipient",
	118: "MailSubmissionFailed",
	119: "MessageReplyFailed",
	120: "AttachmentIsTooLarge",
	121: "UserHasNoMailbox",
	124: "UserDisabledForSync",
	128: "DeviceIsBlockedForThisUser",
	129: "AccessDenied",
	130: "AccountDisabled",
	131: "SyncStateNotFound",
	132: "SyncStateLocked",
	133: "SyncStateCorrupt",
	136: "CommandNotSupported",
	137: "VersionNotSupported",
	138: "DeviceNotFullyProvisionable",
	139: "RemoteWipeRequested",
	140: "LegacyDeviceOnStrictPolicy",
	141: "DeviceNotProvisioned",
	142: "PolicyRefresh",
	143: "InvalidPolicyKey",
}

var commandStatus = map[string]map[int]string{
	"FolderSync": {
		1:  "Success",
		6:  "ServerError (transient; retry, then restart at sync key 0)",
		9:  "SyncKeyMismatch (invalid or stale sync key; restart at sync key 0)",
		10: "IncorrectlyFormattedRequest",
		11: "UnknownError (transient; retry, then restart at sync key 0)",
		12: "CodeUnknown",
	},
	"Sync": {
		1:  "Success",
		3:  "InvalidSyncKey (reset the collection to sync key 0)",
		4:  "ProtocolError",
		5:  "ServerError (transient)",
		6:  "ClientServerConversionError",
		7:  "Conflict",
		8:  "ObjectNotFound",
		9:  "OutOfSpace",
		12: "FolderHierarchyChanged (run FolderSync, then retry)",
		13: "IncompleteRequest",
		14: "InvalidWaitOrHeartbeat (outside the server's range)",
		15: "InvalidSyncCommand",
		16: "RetriableServerError",
	},
	"Ping": {
		1: "HeartbeatExpired (no changes)",
		2: "ChangesOccurred",
		3: "MissingParameters",
		4: "SyntaxError",
		5: "HeartbeatOutOfRange (the response carries the allowed limit)",
		6: "TooManyFolders",
		7: "FolderHierarchySyncRequired (run FolderSync, then retry)",
		8: "ServerError (transient)",
	},
	"Provision": {
		1: "Success",
		2: "ProtocolError",
		3: "ServerError",
	},
	"Provision/Policy": {
		1: "Success",
		2: "NoPolicyForClient",
		3: "UnknownPolicyType",
		4: "ServerDataCorrupt",
		5: "WrongPolicyKey",
	},
	"Settings": {
		1: "Success",
		2: "ProtocolError",
		3: "AccessDenied",
		4: "ServerUnavailable",
		5: "InvalidArguments",
		6: "ConflictingArguments",
		7: "DeniedByPolicy",
	},
	"Search/GAL": {
		1:  "Success",
		2:  "InvalidRequest",
		3:  "ServerError",
		4:  "BadLink",
		5:  "AccessDenied",
		6:  "NotFound",
		7:  "ConnectionFailed",
		8:  "TooComplex",
		10: "IndexNotLoaded",
		11: "Timeout",
	},
}

// StatusName names an EAS status code for the given command ("FolderSync",
// "Sync", "Ping", ...), or says that it is unknown.
func StatusName(command string, code int) string {
	if t, ok := commandStatus[command]; ok {
		if n, ok := t[code]; ok {
			return n
		}
	}
	if n, ok := commonStatus[code]; ok {
		return n
	}
	return "unknown status"
}

// ErrorText is err.Error() with the status names corrected: an EAS status
// error inside err is described with the table of its own command, not with the
// library's shared one. Other errors are returned as they are.
func ErrorText(err error) string {
	if err == nil {
		return ""
	}
	var se *eas.StatusError
	if !errors.As(err, &se) {
		return err.Error()
	}
	fixed := fmt.Sprintf("eas: %s: status %d (%s)", se.Command, se.Code, StatusName(se.Command, se.Code))
	return strings.Replace(err.Error(), se.Error(), fixed, 1)
}

// statusIs reports whether err is an EAS status error of the command with the
// code.
func statusIs(err error, command string, code int) bool {
	var se *eas.StatusError
	return errors.As(err, &se) && se.Command == command && se.Code == code
}
