package app

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// SignInTicketTTL is how long a sign-in ticket can be redeemed (ADR 0085
// decision 3).
const SignInTicketTTL = 60 * time.Second

// Provision says what a sign-in through an unlinked external identity does
// (server.yaml auth.external.provision, ADR 0085 decision 5).
const (
	// ProvisionAuto creates an account with role user and no local password.
	ProvisionAuto = "auto"
	// ProvisionExisting signs in only identities an administrator linked.
	ProvisionExisting = "existing"
)

// Profile is what an identity connector says about a user. It names no role:
// an external identity never makes anyone an administrator.
type Profile struct {
	Username    string
	DisplayName string
	Email       string
}

// IdentityAccount is one account linked through a source, as its connector
// sees it.
type IdentityAccount struct {
	Subject  string
	UserID   uint
	Username string
	// Protected says the account is an administrator: the connector cannot
	// disable, enable, rename or sign it out.
	Protected bool
	Disabled  bool
}

// SignInEntry is a sign-in option a module offers on the sign-in page:
// a button that sends the browser through the core's
// /api/v1/auth/sign-in/start to Start, one of the module's AccessPublic GET
// routes. Label is shown as is; Icon is optional, an icon name the console
// knows.
type SignInEntry struct {
	Source string
	Label  string
	Icon   string
	Start  string
}

// accountsHandle is what the server hands Host.Accounts: the store, the site's
// provisioning policy and the sources each module owns.
type accountsHandle struct {
	st        *store.Store
	provision string
	// owner maps an identity source to the module that declared it.
	owner map[string]string
}

// Accounts is the module's view of the accounts linked through the identity
// sources it declared (Host.DeclareIdentitySources). Every operation takes a
// source and refuses one the module does not own (IDENTITY_SOURCE_CONFLICT);
// none can make an administrator, and none can disable, enable, rename or sign
// out one (IDENTITY_PROTECTED_USER). It is safe for concurrent use; its
// methods do I/O and must not be called from a hook.
type Accounts struct {
	module string
	h      *accountsHandle
}

// Accounts is the module's handle on the accounts of its identity sources. It
// may be called at any time; its operations refuse sources the module did not
// declare while Init ran.
func (h *Host) Accounts() *Accounts {
	return &Accounts{module: h.name, h: h.reg.accounts}
}

var identitySourceName = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)

// DeclareIdentitySources claims identity source names (user_identities.source)
// for this module. A name belongs to one module; claiming one another module
// claimed is IDENTITY_SOURCE_CONFLICT.
func (h *Host) DeclareIdentitySources(sources ...string) error {
	if h.reg.closed {
		return ErrHostClosed
	}
	for _, s := range sources {
		if !identitySourceName.MatchString(s) {
			return fmt.Errorf("extension %s: identity source %q does not match [a-z][a-z0-9_]{1,31}", h.name, s)
		}
		if owner, ok := h.reg.accounts.owner[s]; ok && owner != h.name {
			return messages.NewProductError(messages.ErrorIdentitySourceConflict,
				fmt.Sprintf("identity source %s belongs to extension %s", s, owner), map[string]any{"source": s})
		}
	}
	for _, s := range sources {
		h.reg.accounts.owner[s] = h.name
	}
	return nil
}

// OfferSignIn puts a sign-in entry on the sign-in page. Its source must be one
// the module declared, Start one of its own AccessPublic GET routes, and the
// entry follows the feature of that route: it is left out while the feature is
// locked or hidden. A source has at most one entry.
func (h *Host) OfferSignIn(e SignInEntry) error {
	if h.reg.closed {
		return ErrHostClosed
	}
	if h.reg.accounts.owner[e.Source] != h.name {
		return messages.NewProductError(messages.ErrorIdentitySourceConflict,
			fmt.Sprintf("extension %s did not declare identity source %s", h.name, e.Source), map[string]any{"source": e.Source})
	}
	if strings.TrimSpace(e.Label) == "" || utf8.RuneCountInString(e.Label) > 64 {
		return fmt.Errorf("extension %s: sign-in entry %s needs a label of 1-64 characters", h.name, e.Source)
	}
	if !strings.HasPrefix(e.Start, "/x/"+h.name+"/") && e.Start != "/x/"+h.name {
		return fmt.Errorf("extension %s: sign-in entry %s starts at %q, outside /x/%s", h.name, e.Source, e.Start, h.name)
	}
	for _, prev := range h.reg.signIns {
		if prev.Source == e.Source {
			return fmt.Errorf("extension %s: identity source %s offers a sign-in twice", h.name, e.Source)
		}
	}
	h.reg.signIns = append(h.reg.signIns, api.SignInOption{Source: e.Source, Label: e.Label, Icon: e.Icon, Start: e.Start, Module: h.name})
	return nil
}

var (
	errNotOwned  = errors.New("identity source not owned")
	errProtected = errors.New("protected account")
)

func (a *Accounts) owns(source string) error {
	if a == nil || a.h == nil || a.h.owner[source] != a.module {
		return messages.WrapProductError(messages.ErrorIdentitySourceConflict,
			fmt.Sprintf("extension %s does not own identity source %s", a.moduleName(), source), map[string]any{"source": source}, errNotOwned)
	}
	return nil
}

func (a *Accounts) moduleName() string {
	if a == nil {
		return ""
	}
	return a.module
}

// protectedUser is the refusal for an operation on an administrator.
func protectedUser(source string) error {
	return messages.WrapProductError(messages.ErrorIdentityProtectedUser,
		"an identity connector cannot disable, enable, rename or sign out an administrator", map[string]any{"source": source}, errProtected)
}

// isProtected: administrators, the local administrator among them
// (store.User.IsLocalAdmin), are out of a connector's reach.
func isProtected(u *store.User) bool { return u.Role == "admin" || u.IsLocalAdmin() }

func validProfile(p Profile) error {
	if u := strings.TrimSpace(p.Username); u == "" || u != p.Username || utf8.RuneCountInString(u) > 64 {
		return messages.NewProductError(messages.ErrorExternalIdentityInvalid, "a profile needs a username of 1-64 characters without surrounding spaces", nil)
	}
	if utf8.RuneCountInString(p.DisplayName) > 128 || utf8.RuneCountInString(p.Email) > 254 {
		return messages.NewProductError(messages.ErrorExternalIdentityInvalid, "the profile's display name or email is too long", nil)
	}
	return nil
}

// SignIn is a verified sign-in through source: the core finds the account
// linked to subject, or creates one when the site provisions accounts
// (auth.external.provision: auto), and answers a one-time ticket bound to
// handle, the browser binding the core gave the connector's start route. The
// connector then sends the browser to /api/v1/auth/complete?ticket=<ticket>.
// An account is never found by username or email.
func (a *Accounts) SignIn(source, subject string, p Profile, handle string) (string, error) {
	if err := a.owns(source); err != nil {
		return "", err
	}
	if !validHandle(handle) {
		return "", messages.NewProductError(messages.ErrorIdentityTicketInvalid, "the sign-in did not start at the core's sign-in route", nil)
	}
	if err := validProfile(p); err != nil {
		return "", err
	}
	u, link, err := a.h.st.LinkedAccount(source, subject)
	if errors.Is(err, store.ErrNotFound) {
		if a.h.provision == ProvisionExisting {
			return "", messages.NewProductError(messages.ErrorIdentityNotProvisioned,
				"no account is linked to this identity, and this server creates none", map[string]any{"source": source})
		}
		u, link, err = a.h.st.CreateExternalUser(p.Username, source, subject)
	}
	if err != nil {
		return "", accountError(err, p.Username)
	}
	if u.DisabledAt != nil {
		return "", messages.NewProductError(messages.ErrorUserDisabled, "this account is disabled", nil)
	}
	return a.h.st.IssueSignInTicket(u.ID, link.ID, source, handle, SignInTicketTTL)
}

// Upsert is a directory sync of one member: it creates the account linked to
// subject (whatever the provisioning policy says: the directory is the site's
// own) or renames the linked account to p.Username. Renaming an administrator
// is IDENTITY_PROTECTED_USER.
func (a *Accounts) Upsert(source, subject string, p Profile) (IdentityAccount, error) {
	if err := a.owns(source); err != nil {
		return IdentityAccount{}, err
	}
	if err := validProfile(p); err != nil {
		return IdentityAccount{}, err
	}
	u, _, err := a.h.st.LinkedAccount(source, subject)
	if errors.Is(err, store.ErrNotFound) {
		u, _, err = a.h.st.CreateExternalUser(p.Username, source, subject)
		if err != nil {
			return IdentityAccount{}, accountError(err, p.Username)
		}
		return account(subject, u), nil
	}
	if err != nil {
		return IdentityAccount{}, accountError(err, p.Username)
	}
	if u.Username != p.Username {
		if isProtected(u) {
			return IdentityAccount{}, protectedUser(source)
		}
		if err := a.h.st.RenameUser(u.ID, p.Username); err != nil {
			return IdentityAccount{}, accountError(err, p.Username)
		}
		u.Username = p.Username
	}
	return account(subject, u), nil
}

// Disable disables the account linked to subject (ADR 0085 decision 7).
func (a *Accounts) Disable(source, subject string) error {
	u, err := a.linkedUnprotected(source, subject)
	if err != nil {
		return err
	}
	_, err = a.h.st.DisableUser(u.ID)
	return accountError(err, "")
}

// Enable enables the account linked to subject again.
func (a *Accounts) Enable(source, subject string) error {
	u, err := a.linkedUnprotected(source, subject)
	if err != nil {
		return err
	}
	_, err = a.h.st.EnableUser(u.ID)
	return accountError(err, "")
}

// RevokeSessions ends every management session of the account linked to
// subject, as a sign-out at the identity source asks. It answers how many
// ended.
func (a *Accounts) RevokeSessions(source, subject string) (int, error) {
	u, err := a.linkedUnprotected(source, subject)
	if err != nil {
		return 0, err
	}
	n, err := a.h.st.RevokeUserSessions(u.ID)
	return n, accountError(err, "")
}

// MaxAccountsPage bounds one page of Accounts.List.
const MaxAccountsPage = 500

// List pages through the accounts linked through source, for a full
// directory sync's reconciliation. cursor is 0 for the first page and then
// the next List answered; next is 0 after the last page.
func (a *Accounts) List(source string, cursor uint, limit int) (accounts []IdentityAccount, next uint, err error) {
	if err := a.owns(source); err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > MaxAccountsPage {
		return nil, 0, fmt.Errorf("accounts: limit %d is outside 1-%d", limit, MaxAccountsPage)
	}
	rows, next, err := a.h.st.AccountsBySource(source, cursor, limit)
	if err != nil {
		return nil, 0, err
	}
	out := make([]IdentityAccount, len(rows))
	for i, r := range rows {
		out[i] = IdentityAccount{Subject: r.Subject, UserID: r.UserID, Username: r.Username,
			Protected: r.Protected, Disabled: r.Disabled}
	}
	return out, next, nil
}

func (a *Accounts) linkedUnprotected(source, subject string) (*store.User, error) {
	if err := a.owns(source); err != nil {
		return nil, err
	}
	u, _, err := a.h.st.LinkedAccount(source, subject)
	if err != nil {
		return nil, accountError(err, "")
	}
	if isProtected(u) {
		return nil, protectedUser(source)
	}
	return u, nil
}

func account(subject string, u *store.User) IdentityAccount {
	return IdentityAccount{Subject: subject, UserID: u.ID, Username: u.Username, Protected: isProtected(u), Disabled: u.DisabledAt != nil}
}

// accountError turns the store's account errors into product errors.
func accountError(err error, username string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return messages.WrapProductError(messages.ErrorExternalIdentityNotFound, "no account is linked to this identity", nil, err)
	case errors.Is(err, store.ErrUsernameTaken):
		return messages.WrapProductError(messages.ErrorIdentityUsernameTaken, "another account already has this username",
			map[string]any{"username": username}, err)
	case errors.Is(err, store.ErrIdentityTaken):
		return messages.WrapProductError(messages.ErrorExternalIdentityTaken, "the external identity is already linked", nil, err)
	case errors.Is(err, store.ErrInvalidIdentity):
		return messages.WrapProductError(messages.ErrorExternalIdentityInvalid, "the source or subject is not a valid external identity", nil, err)
	case errors.Is(err, store.ErrUserDisabled):
		return messages.WrapProductError(messages.ErrorUserDisabled, "this account is disabled", nil, err)
	}
	return err
}

// validHandle: a handle the core minted is 64 lowercase hex characters.
func validHandle(h string) bool {
	if len(h) != 64 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
