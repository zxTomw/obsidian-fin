// Package plaid isolates the official SDK, retries and sanitized provider errors.
package plaid

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	sdk "github.com/plaid/plaid-go/v45/plaid"
	"obsidian-fin/internal/domain"
	"obsidian-fin/internal/errs"
	"obsidian-fin/internal/money"
)

type LinkRequest struct {
	UserID, Country string
	HistoryDays     int
	AccessToken     string
}
type Link struct {
	Token, URL string
	Expires    time.Time
}
type Completion struct {
	PublicToken       string
	Success, Finished bool
}
type Exchange struct{ ItemID, AccessToken string }
type Metadata struct {
	ItemID, InstitutionID, InstitutionName string
	Accounts                               []domain.Account
}

type Client struct {
	api     *sdk.APIClient
	country string
}
type captureKey struct{}
type capture struct{ body []byte }
type captureTransport struct{ base http.RoundTripper }

func (t captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if c, ok := req.Context().Value(captureKey{}).(*capture); ok && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20+1))
		resp.Body.Close()
		if readErr != nil || len(body) > 32<<20 {
			return nil, errors.New("could not read bounded Plaid response")
		}
		c.body = body
		resp.Body = io.NopCloser(bytes.NewReader(body))
	}
	return resp, nil
}

func New(environment, clientID, secret, country string) (*Client, error) {
	if environment != "sandbox" && environment != "production" {
		return nil, errs.New(2, "invalid Plaid environment")
	}
	if clientID == "" || secret == "" {
		return nil, errs.New(2, "Plaid client ID and Keychain secret are required")
	}
	configuration := sdk.NewConfiguration()
	configuration.AddDefaultHeader("PLAID-CLIENT-ID", clientID)
	configuration.AddDefaultHeader("PLAID-SECRET", secret)
	configuration.AddDefaultHeader("Plaid-Version", "2020-09-14")
	if environment == "production" {
		configuration.UseEnvironment(sdk.Production)
	} else {
		configuration.UseEnvironment(sdk.Sandbox)
	}
	configuration.Debug = false
	configuration.HTTPClient = &http.Client{Timeout: 60 * time.Second, Transport: captureTransport{base: http.DefaultTransport}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Client{api: sdk.NewAPIClient(configuration), country: country}, nil
}

func wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func safeError(err error, resp *http.Response) error {
	if err == nil {
		return nil
	}
	var apiErr interface {
		error
		Body() []byte
	}
	if errors.As(err, &apiErr) {
		var payload struct {
			Code      string `json:"error_code"`
			RequestID string `json:"request_id"`
		}
		if json.Unmarshal(apiErr.Body(), &payload) == nil && payload.Code != "" {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			return &errs.APIError{Code: errs.SafeIdentifier(payload.Code), RequestID: errs.SafeIdentifier(payload.RequestID), Status: status}
		}
	}
	return errs.New(1, "Plaid network or response error; rerun the command")
}

func retryDelay(attempt int, resp *http.Response) (time.Duration, bool) {
	delay := time.Duration(1<<attempt) * time.Second
	var jitter [2]byte
	if _, err := rand.Read(jitter[:]); err == nil {
		delay += time.Duration(binary.BigEndian.Uint16(jitter[:])%500) * time.Millisecond
	}
	if resp != nil {
		header := resp.Header.Get("Retry-After")
		if seconds, err := strconv.ParseUint(header, 10, 32); err == nil {
			if seconds > 60 {
				return 0, false
			}
			if d := time.Duration(seconds) * time.Second; d > delay {
				delay = d
			}
		} else if when, err := http.ParseTime(header); err == nil {
			d := time.Until(when)
			if d > 60*time.Second {
				return 0, false
			}
			if d > delay {
				delay = d
			}
		}
	}
	return delay, true
}

func request[T any](ctx context.Context, retry bool, fn func(context.Context) (T, *http.Response, error)) (T, error) {
	var zero T
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		callCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		value, resp, err := fn(callCtx)
		cancel()
		if err == nil {
			return value, nil
		}
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		retryable := resp == nil || resp.StatusCode == 429 || resp.StatusCode >= 500
		if !retry || !retryable || attempt >= 3 {
			return zero, safeError(err, resp)
		}
		delay, allowed := retryDelay(attempt, resp)
		if !allowed {
			return zero, safeError(err, resp)
		}
		if err := wait(ctx, delay); err != nil {
			return zero, err
		}
	}
}

func (c *Client) CreateLink(ctx context.Context, in LinkRequest) (Link, error) {
	req := sdk.NewLinkTokenCreateRequest("obsidian-fin", "en", []sdk.CountryCode{sdk.CountryCode(in.Country)})
	req.SetUser(*sdk.NewLinkTokenCreateRequestUser(in.UserID))
	req.HostedLink = &sdk.LinkTokenCreateHostedLink{}
	req.EnableMultiItemLink = sdk.PtrBool(false)
	if in.AccessToken != "" {
		req.SetAccessToken(in.AccessToken)
	} else {
		req.Products = []sdk.Products{sdk.Products("transactions")}
		days := int32(in.HistoryDays)
		req.Transactions = &sdk.LinkTokenTransactions{DaysRequested: &days}
	}
	result, err := request(ctx, true, func(ctx context.Context) (sdk.LinkTokenCreateResponse, *http.Response, error) {
		return c.api.PlaidApi.LinkTokenCreate(ctx).LinkTokenCreateRequest(*req).Execute()
	})
	if err != nil {
		return Link{}, err
	}
	link := Link{Token: result.LinkToken, URL: result.GetHostedLinkUrl(), Expires: result.Expiration}
	u, parseErr := url.Parse(link.URL)
	if link.Token == "" || parseErr != nil || u.Scheme != "https" || u.Hostname() != "secure.plaid.com" || u.User != nil {
		return Link{}, errs.New(1, "Plaid did not return a valid Hosted Link; verify Hosted Link access")
	}
	return link, nil
}

func (c *Client) Completion(ctx context.Context, token string, update bool) (Completion, error) {
	result, err := request(ctx, true, func(ctx context.Context) (sdk.LinkTokenGetResponse, *http.Response, error) {
		return c.api.PlaidApi.LinkTokenGet(ctx).LinkTokenGetRequest(*sdk.NewLinkTokenGetRequest(token)).Execute()
	})
	if err != nil {
		return Completion{}, err
	}
	for _, session := range result.GetLinkSessions() {
		if update {
			if session.OnSuccess.Get() != nil {
				return Completion{Success: true, Finished: true}, nil
			}
		} else if results := session.Results.Get(); results != nil && len(results.ItemAddResults) > 0 {
			if len(results.ItemAddResults) != 1 {
				return Completion{}, errs.New(1, "Hosted Link returned multiple Items; only one Item is supported")
			}
			if public := results.ItemAddResults[0].PublicToken; public != "" {
				return Completion{PublicToken: public, Success: true, Finished: true}, nil
			}
		}
		if exit := session.Exit.Get(); exit != nil {
			if providerError := exit.Error.Get(); providerError != nil {
				return Completion{}, &errs.APIError{Code: string(providerError.ErrorCode), RequestID: providerError.GetRequestId()}
			}
			return Completion{Finished: true}, nil
		}
		if exit := session.OnExit.Get(); exit != nil {
			if providerError := exit.Error.Get(); providerError != nil {
				return Completion{}, &errs.APIError{Code: string(providerError.ErrorCode), RequestID: providerError.GetRequestId()}
			}
			return Completion{Finished: true}, nil
		}
		// finished_at alone is not evidence of success; results can arrive on a later poll.
	}
	return Completion{}, nil
}

func (c *Client) Exchange(ctx context.Context, token string) (Exchange, error) {
	// A transport failure can hide a completed exchange. Never blindly retry it.
	result, err := request(ctx, false, func(ctx context.Context) (sdk.ItemPublicTokenExchangeResponse, *http.Response, error) {
		return c.api.PlaidApi.ItemPublicTokenExchange(ctx).ItemPublicTokenExchangeRequest(*sdk.NewItemPublicTokenExchangeRequest(token)).Execute()
	})
	if err != nil {
		return Exchange{}, err
	}
	return Exchange{ItemID: result.ItemId, AccessToken: result.AccessToken}, nil
}

func (c *Client) Metadata(ctx context.Context, token string) (Metadata, error) {
	item, err := request(ctx, true, func(ctx context.Context) (sdk.ItemGetResponse, *http.Response, error) {
		return c.api.PlaidApi.ItemGet(ctx).ItemGetRequest(*sdk.NewItemGetRequest(token)).Execute()
	})
	if err != nil {
		return Metadata{}, err
	}
	id := item.Item.GetInstitutionId()
	if id == "" {
		return Metadata{}, errs.New(1, "Plaid Item has no institution; verify the selected BMO institution")
	}
	if providerErr := item.Item.Error.Get(); providerErr != nil {
		return Metadata{}, &errs.APIError{Code: string(providerErr.ErrorCode), RequestID: providerErr.GetRequestId()}
	}
	institution, err := request(ctx, true, func(ctx context.Context) (sdk.InstitutionsGetByIdResponse, *http.Response, error) {
		return c.api.PlaidApi.InstitutionsGetById(ctx).InstitutionsGetByIdRequest(*sdk.NewInstitutionsGetByIdRequest(id, []sdk.CountryCode{sdk.CountryCode(c.country)})).Execute()
	})
	if err != nil {
		return Metadata{}, err
	}
	name := institution.Institution.Name
	countrySupported := false
	for _, country := range institution.Institution.CountryCodes {
		if string(country) == c.country {
			countrySupported = true
		}
	}
	if !countrySupported {
		return Metadata{}, errs.New(1, "selected BMO institution does not match the configured country; verify institution coverage")
	}
	upper := strings.ToUpper(name)
	if !strings.Contains(upper, "BMO") && !strings.Contains(upper, "BANK OF MONTREAL") {
		return Metadata{}, errs.New(1, "selected institution is not BMO; choose the BMO institution for the configured country")
	}
	supported := false
	for _, product := range institution.Institution.Products {
		if string(product) == "transactions" {
			supported = true
		}
	}
	if !supported {
		return Metadata{}, errs.New(1, "selected BMO institution does not provide Plaid Transactions; verify coverage and product access")
	}
	accounts, err := request(ctx, true, func(ctx context.Context) (sdk.AccountsGetResponse, *http.Response, error) {
		return c.api.PlaidApi.AccountsGet(ctx).AccountsGetRequest(*sdk.NewAccountsGetRequest(token)).Execute()
	})
	if err != nil {
		return Metadata{}, err
	}
	m := Metadata{ItemID: item.Item.ItemId, InstitutionID: id, InstitutionName: name}
	for _, a := range accounts.Accounts {
		out := domain.Account{ID: a.AccountId, Name: a.Name, OfficialName: a.OfficialName.Get(), Mask: a.Mask.Get(), Type: string(a.Type)}
		if subtype := a.Subtype.Get(); subtype != nil {
			v := string(*subtype)
			out.Subtype = &v
		}
		m.Accounts = append(m.Accounts, out)
	}
	if len(m.Accounts) == 0 {
		return Metadata{}, errs.New(1, "Plaid returned no authorized accounts; rerun connect with account access")
	}
	return m, nil
}

func (c *Client) Remove(ctx context.Context, token string) error {
	_, err := request(ctx, true, func(ctx context.Context) (sdk.ItemRemoveResponse, *http.Response, error) {
		return c.api.PlaidApi.ItemRemove(ctx).ItemRemoveRequest(*sdk.NewItemRemoveRequest(token)).Execute()
	})
	return err
}

// rawTransaction intentionally decodes amount as json.Number. The SDK's float64 amounts are never used.
type rawTransaction struct {
	ID                 string       `json:"transaction_id"`
	AccountID          *string      `json:"account_id"`
	PendingID          *string      `json:"pending_transaction_id"`
	Date               *string      `json:"date"`
	AuthorizedDate     *string      `json:"authorized_date"`
	Name               *string      `json:"name"`
	MerchantName       *string      `json:"merchant_name"`
	Amount             *json.Number `json:"amount"`
	Currency           *string      `json:"iso_currency_code"`
	UnofficialCurrency *string      `json:"unofficial_currency_code"`
	Pending            *bool        `json:"pending"`
	Category           *struct {
		Primary    *string `json:"primary"`
		Detail     *string `json:"detailed"`
		Confidence *string `json:"confidence_level"`
	} `json:"personal_finance_category"`
}

func (t rawTransaction) domain() (domain.Transaction, error) {
	out := domain.Transaction{ID: t.ID, AccountID: t.AccountID, PendingID: t.PendingID, Date: t.Date, AuthorizedDate: t.AuthorizedDate, Name: t.Name, MerchantName: t.MerchantName, Currency: t.Currency, UnofficialCurrency: t.UnofficialCurrency, Pending: t.Pending}
	if t.Amount == nil {
		return out, errs.New(1, "Plaid returned a transaction without an amount")
	}
	v, err := money.Canonical(t.Amount.String())
	if err != nil {
		return out, errs.New(1, "Plaid returned an invalid decimal amount")
	}
	out.Amount = &v
	if t.Category != nil {
		out.CategoryPrimary = t.Category.Primary
		out.CategoryDetail = t.Category.Detail
		out.CategoryConfidence = t.Category.Confidence
	}
	return out, nil
}
func (c *Client) Sync(ctx context.Context, token string, cursor *string) (domain.Page, error) {
	req := sdk.NewTransactionsSyncRequest(token)
	req.Count = sdk.PtrInt32(500)
	if cursor != nil {
		req.SetCursor(*cursor)
	}
	var raw capture
	_, err := request(ctx, true, func(ctx context.Context) (sdk.TransactionsSyncResponse, *http.Response, error) {
		raw.body = nil
		return c.api.PlaidApi.TransactionsSync(context.WithValue(ctx, captureKey{}, &raw)).TransactionsSyncRequest(*req).Execute()
	})
	if err != nil {
		return domain.Page{}, err
	}
	return decodePage(raw.body)
}
func decodePage(body []byte) (domain.Page, error) {
	var raw struct {
		Accounts []struct {
			ID           string  `json:"account_id"`
			Name         string  `json:"name"`
			OfficialName *string `json:"official_name"`
			Mask         *string `json:"mask"`
			Type         string  `json:"type"`
			Subtype      *string `json:"subtype"`
		} `json:"accounts"`
		Added    []rawTransaction `json:"added"`
		Modified []rawTransaction `json:"modified"`
		Removed  []struct {
			ID string `json:"transaction_id"`
		} `json:"removed"`
		NextCursor *string `json:"next_cursor"`
		HasMore    *bool   `json:"has_more"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.NextCursor == nil || raw.HasMore == nil {
		return domain.Page{}, errs.New(1, "Plaid returned an invalid sync response; rerun sync")
	}
	p := domain.Page{NextCursor: *raw.NextCursor, HasMore: *raw.HasMore}
	for _, a := range raw.Accounts {
		p.Accounts = append(p.Accounts, domain.Account{ID: a.ID, Name: a.Name, OfficialName: a.OfficialName, Mask: a.Mask, Type: a.Type, Subtype: a.Subtype})
	}
	for _, t := range raw.Added {
		out, err := t.domain()
		if err != nil {
			return p, err
		}
		p.Added = append(p.Added, out)
	}
	for _, t := range raw.Modified {
		out, err := t.domain()
		if err != nil {
			return p, err
		}
		p.Modified = append(p.Modified, out)
	}
	for _, t := range raw.Removed {
		p.Removed = append(p.Removed, t.ID)
	}
	return p, nil
}
