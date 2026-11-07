package plugin

import (
	"net/http"

	"cpa-super-billing/internal/billing"
	"cpa-super-billing/internal/messages"
)

// databaseAlertHeader marks every management response while the database's
// write-ahead log files are deleted, so each admin page can show the alert.
const databaseAlertHeader = "X-Billing-Database-Alert"

const (
	databaseAlertCheckpointed     = "wal-deleted"
	databaseAlertCheckpointFailed = "wal-deleted-checkpoint-failed"
)

// databaseAlertView is the alert the subscription split page shows.
type databaseAlertView struct {
	billing.DatabaseFileAlert
	Message messages.Message `json:"message"`
	Text    string           `json:"text"`
}

func databaseAlertState(alert billing.DatabaseFileAlert) string {
	if alert.CheckpointError != "" || (alert.Checkpoint != nil && alert.Checkpoint.Busy != 0) {
		return databaseAlertCheckpointFailed
	}
	return databaseAlertCheckpointed
}

func newDatabaseAlertView(alert billing.DatabaseFileAlert) databaseAlertView {
	var message messages.Message
	if databaseAlertState(alert) == databaseAlertCheckpointFailed {
		message = messages.New("The write-ahead log files of the billing database were deleted while the plugin was running (%s), "+
			"and recent writes exist only in the deleted files. Copying them back into the database file has not succeeded yet "+
			"and is retried every minute. Stop CLIProxyAPI gracefully (docker compose stop or a normal service stop) and start it again; "+
			"do not kill it", alert.Files())
	} else {
		message = messages.New("The write-ahead log files of the billing database were deleted while the plugin was running (%s), "+
			"and recent writes exist only in the deleted files. The plugin has copied them back into the database file. "+
			"Stop CLIProxyAPI gracefully (docker compose stop or a normal service stop) and start it again; do not kill it", alert.Files())
	}
	return databaseAlertView{DatabaseFileAlert: alert, Message: message, Text: message.Text}
}

// withDatabaseAlert marks an admin management response while the alert lasts.
func (a *App) withDatabaseAlert(response ManagementResponse) ManagementResponse {
	alert, ok := a.store.DatabaseFileAlert()
	if !ok {
		return response
	}
	headers := response.Headers.Clone()
	if headers == nil {
		headers = make(http.Header, 1)
	}
	headers.Set(databaseAlertHeader, databaseAlertState(alert))
	response.Headers = headers
	return response
}
