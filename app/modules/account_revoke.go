package modules

import "mimamori/app/models"

// RevokeAccountAuth ends what lets an account in: its login sessions (all,
// or all but keepSession, the hashed id of the session that made the
// change), the re-authentication of the session it keeps, its browser
// verifications (the browsers that need no login code) and its pending
// verification codes. It is used when the password or the e-mail address
// changes and when the account is disabled.
func RevokeAccountAuth(db models.Writer, accountID int64, keepSession string) error {
	if err := models.DeleteAccountSessions(db, accountID, keepSession); err != nil {
		return err
	}
	if err := models.ClearAccountStepUps(db, accountID); err != nil {
		return err
	}
	if err := models.DeleteAccountBrowserVerifications(db, accountID); err != nil {
		return err
	}
	return models.DeleteAccountAuthCodes(db, accountID, "")
}
