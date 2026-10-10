// Package domain contains provider-independent imported data. It never holds balances.
package domain

import "time"

type Item struct {
	ID, Environment, InstitutionID, InstitutionName, TokenRef, ClientUserID string
	Cursor                                                                  *string
	ConnectedAt                                                             time.Time
	LastSyncAt                                                              *time.Time
}

type Account struct {
	ID, Name, Type              string
	OfficialName, Mask, Subtype *string
}

type Transaction struct {
	ID                                                  string
	AccountID, NoteID, PendingID, SupersededBy          *string
	Date, AuthorizedDate, Name, MerchantName, Amount    *string
	Currency, UnofficialCurrency                        *string
	Pending                                             *bool
	CategoryPrimary, CategoryDetail, CategoryConfidence *string
	Removed                                             bool
}

type Page struct {
	Accounts        []Account
	Added, Modified []Transaction
	Removed         []string
	NextCursor      string
	HasMore         bool
}

type Note struct {
	ID, ItemID, Environment string
	Transaction             Transaction
	Account                 Account
	Export                  Export
}

type Export struct {
	Path                   string
	BlockHash, PendingHash *string
	TemplateVersion        int
	ExportedAt             *time.Time
}

type Recovery struct {
	ItemID, TokenRef, ClientUserID string
	Revoked                        bool
}
