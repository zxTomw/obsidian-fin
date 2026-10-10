package plaid

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	sdk "github.com/plaid/plaid-go/v45/plaid"
	"obsidian-fin/internal/errs"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}
func clientFor(t *testing.T, fn roundTrip) *Client {
	t.Helper()
	configuration := sdk.NewConfiguration()
	configuration.UseEnvironment(sdk.Sandbox)
	configuration.HTTPClient = &http.Client{Transport: captureTransport{base: fn}, Timeout: 60 * time.Second}
	return &Client{api: sdk.NewAPIClient(configuration), country: "CA"}
}

func TestHostedLinkInitialAndUpdateRequests(t *testing.T) {
	for _, update := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "update"}[update], func(t *testing.T) {
			c := clientFor(t, func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/link/token/create" || req.URL.Scheme != "https" {
					t.Fatal("unexpected endpoint")
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if _, ok := body["hosted_link"]; !ok {
					t.Fatal("Hosted Link disabled")
				}
				if _, ok := body["webhook"]; ok {
					t.Fatal("unscoped webhook")
				}
				if update {
					if _, ok := body["products"]; ok {
						t.Fatal("update includes products")
					}
					if _, ok := body["transactions"]; ok {
						t.Fatal("update includes history")
					}
					if !strings.Contains(string(body["access_token"]), "access-sandbox-test") {
						t.Fatal("update token missing")
					}
				} else {
					if string(body["products"]) != `["transactions"]` || !strings.Contains(string(body["transactions"]), `"days_requested":90`) {
						t.Fatal("initial product/history missing")
					}
					if _, ok := body["access_token"]; ok {
						t.Fatal("initial access token set")
					}
				}
				return response(200, `{"link_token":"link-sandbox-test","hosted_link_url":"https://secure.plaid.com/hl/test","expiration":"2026-10-10T12:00:00Z","request_id":"request-test"}`), nil
			})
			request := LinkRequest{UserID: "stable-user", Country: "CA", HistoryDays: 90}
			if update {
				request.AccessToken = "access-sandbox-test"
			}
			link, err := c.CreateLink(context.Background(), request)
			if err != nil || link.Token != "link-sandbox-test" {
				t.Fatalf("link=%+v err=%v", link, err)
			}
		})
	}
}

func TestCompletionUsesResultsAndUpdateSuccess(t *testing.T) {
	for _, tc := range []struct {
		name, body                string
		update, success, finished bool
		public                    string
	}{
		{"initial results", `{"link_sessions":[{"link_session_id":"test","results":{"item_add_results":[{"public_token":"public-sandbox-test"}]}}]}`, false, true, true, "public-sandbox-test"},
		{"update", `{"link_sessions":[{"link_session_id":"test","on_success":{"public_token":""},"finished_at":"2026-10-10T12:00:00Z"}]}`, true, true, true, ""},
		{"exit", `{"link_sessions":[{"link_session_id":"test","exit":{"error":null,"metadata":null}}]}`, false, false, true, ""},
		{"legacy exit", `{"link_sessions":[{"link_session_id":"test","on_exit":{}}]}`, true, false, true, ""},
		{"finished results delayed", `{"link_sessions":[{"link_session_id":"test","finished_at":"2026-10-10T12:00:00Z"}]}`, false, false, false, ""},
		{"waiting", `{"link_sessions":[]}`, false, false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := clientFor(t, func(*http.Request) (*http.Response, error) { return response(200, tc.body), nil })
			got, err := c.Completion(context.Background(), "link-sandbox-test", tc.update)
			if err != nil || got.Success != tc.success || got.Finished != tc.finished || got.PublicToken != tc.public {
				t.Fatalf("got %+v err=%v", got, err)
			}
		})
	}
}

func TestSyncPreservesDecimalNullsAndOmitsInitialCursor(t *testing.T) {
	for _, cursor := range []*string{nil, ptr("saved")} {
		c := clientFor(t, func(req *http.Request) (*http.Response, error) {
			var body map[string]json.RawMessage
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			_, present := body["cursor"]
			if present != (cursor != nil) {
				t.Fatal("initial cursor not omitted")
			}
			if cursor != nil && string(body["cursor"]) != `"saved"` {
				t.Fatal("cursor not forwarded")
			}
			return response(200, `{"transactions_update_status":"HISTORICAL_UPDATE_COMPLETE","accounts":[{"account_id":"a","name":"Example","type":"depository","mask":null,"subtype":null,"official_name":null,"balances":{"current":12345}}],"added":[{"transaction_id":"t","account_id":"a","amount":9007199254740993.01234567890123456789,"date":"2026-10-09","authorized_date":null,"name":"Example","merchant_name":null,"iso_currency_code":null,"unofficial_currency_code":null,"pending":false,"personal_finance_category":{"primary":"GENERAL_MERCHANDISE","detailed":"GENERAL_MERCHANDISE_OTHER","confidence_level":"HIGH"}}],"modified":[],"removed":[],"next_cursor":"next","has_more":false,"request_id":"request-test"}`), nil
		})
		page, err := c.Sync(context.Background(), "access-sandbox-test", cursor)
		if err != nil {
			t.Fatal(err)
		}
		txn := page.Added[0]
		if *txn.Amount != "9007199254740993.01234567890123456789" || txn.Currency != nil || txn.MerchantName != nil || txn.AuthorizedDate != nil {
			t.Fatal("decimal/null data lost")
		}
		if len(page.Accounts) != 1 || page.Accounts[0].Mask != nil || page.NextCursor != "next" {
			t.Fatal("metadata/cursor lost")
		}
	}
}
func ptr(s string) *string { return &s }

func TestRetryBoundedRateLimitAndExchangeNotRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		c := clientFor(t, func(*http.Request) (*http.Response, error) {
			calls++
			r := response(429, `{"error_code":"RATE_LIMIT_EXCEEDED","request_id":"request-test","error_message":"secret-sandbox-DO-NOT-LOG"}`)
			r.Header.Set("Retry-After", "2")
			return r, nil
		})
		start := time.Now()
		_, err := c.Completion(context.Background(), "link-sandbox-DO-NOT-LOG", false)
		if calls != 4 || !errs.IsAPI(err, "RATE_LIMIT_EXCEEDED") || time.Since(start) < 8*time.Second {
			t.Fatalf("calls=%d err=%v delay=%s", calls, err, time.Since(start))
		}
		if strings.Contains(err.Error(), "DO-NOT-LOG") {
			t.Fatal("API error leaked body/token")
		}
		calls = 0
		c = clientFor(t, func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("network failed at https://private.invalid/access-sandbox-DO-NOT-LOG")
		})
		_, err = c.Exchange(context.Background(), "public-sandbox-DO-NOT-LOG")
		if calls != 1 || strings.Contains(err.Error(), "DO-NOT-LOG") {
			t.Fatal("ambiguous exchange retried or leaked")
		}
	})
}

func TestRetryNetwork5xxCancellationAndTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		c := clientFor(t, func(req *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("temporary")
			}
			if calls == 2 {
				return response(503, `{"error_code":"INTERNAL_SERVER_ERROR","request_id":"test"}`), nil
			}
			return response(200, `{"request_id":"test"}`), nil
		})
		if err := c.Remove(context.Background(), "access-sandbox-test"); err != nil || calls != 3 {
			t.Fatalf("retry=%d %v", calls, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		calls = 0
		c = clientFor(t, func(*http.Request) (*http.Response, error) { calls++; cancel(); return nil, errors.New("network") })
		if err := c.Remove(ctx, "token"); !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatal("cancellation retried")
		}
		calls = 0
		start := time.Now()
		c = clientFor(t, func(req *http.Request) (*http.Response, error) {
			calls++
			<-req.Context().Done()
			return nil, req.Context().Err()
		})
		if err := c.Remove(context.Background(), "token"); err == nil || calls != 4 || time.Since(start) < 240*time.Second {
			t.Fatal("request timeout/retry not bounded")
		}
	})
}

func TestSanitizedAPIErrorAndUnsafeHostedURL(t *testing.T) {
	c := clientFor(t, func(*http.Request) (*http.Response, error) {
		return response(400, `{"error_code":"ITEM_LOGIN_REQUIRED","request_id":"safe_request","error_message":"access-sandbox-private secret-private account-private","display_message":"private"}`), nil
	})
	_, err := c.Completion(context.Background(), "link-sandbox-private", false)
	if err == nil || !strings.Contains(err.Error(), "safe_request") || !strings.Contains(err.Error(), "connect --update") || strings.Contains(err.Error(), "private") {
		t.Fatal("unsafe/incomplete public API error")
	}
	c = clientFor(t, func(*http.Request) (*http.Response, error) {
		return response(200, `{"link_token":"link-sandbox-private","hosted_link_url":"https://evil.invalid/private","expiration":"2026-10-10T12:00:00Z"}`), nil
	})
	if _, err := c.CreateLink(context.Background(), LinkRequest{Country: "CA"}); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("unsafe Hosted URL accepted or leaked")
	}
}

func TestMetadataChecksBMOAndDiscardsBalances(t *testing.T) {
	for _, kind := range []string{"valid", "wrong bank", "wrong country", "no transactions", "login required"} {
		t.Run(kind, func(t *testing.T) {
			c := clientFor(t, func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case "/item/get":
					if kind == "login required" {
						return response(200, `{"item":{"item_id":"item-test","institution_id":"ins-test","error":{"error_code":"ITEM_LOGIN_REQUIRED","request_id":"safe-request","error_message":"private account balance"}}}`), nil
					}
					return response(200, `{"item":{"item_id":"item-test","institution_id":"ins-test","error":null}}`), nil
				case "/institutions/get_by_id":
					name := "BMO Test"
					country := "CA"
					product := "transactions"
					if kind == "wrong bank" {
						name = "Other bank"
					}
					if kind == "wrong country" {
						country = "US"
					}
					if kind == "no transactions" {
						product = "auth"
					}
					body, err := json.Marshal(map[string]any{"institution": map[string]any{"institution_id": "ins-test", "name": name, "country_codes": []string{country}, "products": []string{product}, "oauth": true}})
					if err != nil {
						t.Fatal(err)
					}
					return response(200, string(body)), nil
				case "/accounts/get":
					return response(200, `{"item":{"item_id":"item-test"},"accounts":[{"account_id":"a","name":"Test account","official_name":null,"type":"depository","subtype":"checking","mask":"1234","balances":{"current":987654.321,"available":987000.000}}]}`), nil
				default:
					t.Fatalf("unscoped metadata endpoint: %s", req.URL.Path)
					return nil, nil
				}
			})
			metadata, err := c.Metadata(context.Background(), "access-sandbox-test")
			if kind != "valid" {
				if err == nil {
					t.Fatal("invalid metadata accepted")
				}
				if strings.Contains(err.Error(), "balance") {
					t.Fatal("provider message leaked")
				}
				if kind == "login required" && !strings.Contains(err.Error(), "connect --update") {
					t.Fatal("login-required remedy lost")
				}
				return
			}
			if err != nil || metadata.InstitutionName != "BMO Test" || len(metadata.Accounts) != 1 || metadata.Accounts[0].OfficialName != nil {
				t.Fatalf("metadata=%+v err=%v", metadata, err)
			}
			body, err := json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "987654") || strings.Contains(string(body), "balances") {
				t.Fatal("balance crossed adapter boundary")
			}
		})
	}
}

func TestLongRetryAfterFailsWithoutEarlyRetry(t *testing.T) {
	calls := 0
	c := clientFor(t, func(*http.Request) (*http.Response, error) {
		calls++
		r := response(429, `{"error_code":"RATE_LIMIT_EXCEEDED","request_id":"test"}`)
		r.Header.Set("Retry-After", "120")
		return r, nil
	})
	if err := c.Remove(context.Background(), "token"); !errs.IsAPI(err, "RATE_LIMIT_EXCEEDED") || calls != 1 {
		t.Fatal("retried sooner than Retry-After allowed")
	}
}

func TestHostedSessionErrorsAreNotUserCancellation(t *testing.T) {
	for _, field := range []string{"exit", "on_exit"} {
		t.Run(field, func(t *testing.T) {
			body := `{"link_sessions":[{"link_session_id":"test","` + field + `":{"error":{"error_code":"ITEM_LOGIN_REQUIRED","request_id":"safe-request","error_message":"private credentials and account data"}}}]}`
			c := clientFor(t, func(*http.Request) (*http.Response, error) { return response(200, body), nil })
			_, err := c.Completion(context.Background(), "link-sandbox-test", true)
			if errs.ExitCode(err) != 1 || !errs.IsAPI(err, "ITEM_LOGIN_REQUIRED") || !strings.Contains(err.Error(), "connect --update") || strings.Contains(err.Error(), "private") {
				t.Fatal("provider exit error was canceled or leaked")
			}
		})
	}
}
