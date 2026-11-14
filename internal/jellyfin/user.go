package jellyfin

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gorilla/mux"
	"golang.org/x/crypto/bcrypt"

	"github.com/adhikariprivate/rcfin-server/internal/database/model"
)

const (
	ErrUserIDNotFound = "userid not found"
)

// GET /Users
//
// usersGetHandler returns all users
func (j *Jellyfin) usersGetHandler(w http.ResponseWriter, r *http.Request) {
	reqCtx := j.getRequestCtx(w, r)
	if reqCtx == nil {
		return
	}
	// Get all users
	users, err := j.repo.GetAllUsers(r.Context())
	if err != nil {
		apierror(w, "failed to get users", http.StatusInternalServerError)
		return
	}

	queryParams := r.URL.Query()
	filterIsHidden := queryParams.Get("isHidden")
	filterIsDisabled := queryParams.Get("isDisabled")

	response := make([]JFUser, 0, len(users))
	for _, dbuser := range users {
		user := j.makeJFUser(r.Context(), &dbuser)
		// Apply isHidden filter if specified
		if filterIsHidden != "" {
			wantHidden := filterIsHidden == "true"
			if user.Policy.IsHidden != wantHidden {
				continue
			}
		}
		// Apply isDisabled filter if specified
		if filterIsDisabled != "" {
			wantDisabled := filterIsDisabled == "true"
			if user.Policy.IsDisabled != wantDisabled {
				continue
			}
		}
		// Include user if requestor is the user
		if reqCtx.User.ID == user.Id ||
			// Include user if requestor has administrator privileges
			reqCtx.User.Properties.Admin ||
			// Include user if user is public
			!user.Policy.IsHidden {
			response = append(response, user)
		}
	}
	serveJSON(response, w)
}

// POST /Users?userId=u_VvbHefPib3oOdj8d4hUW
//
// usersPostHandler updates a user
func (j *Jellyfin) usersPostHandler(w http.ResponseWriter, r *http.Request) {
	reqCtx := j.getRequestCtx(w, r)
	if reqCtx == nil {
		return
	}
	apierror(w, "account administration is available through the RCFin CLI", http.StatusForbidden)
}

// GET /Users/Me
//
// usersMeHandler returns the current user
func (j *Jellyfin) usersMeHandler(w http.ResponseWriter, r *http.Request) {
	reqCtx := j.getRequestCtx(w, r)
	if reqCtx == nil {
		return
	}
	response := j.makeJFUser(r.Context(), reqCtx.User)
	serveJSON(response, w)
}

// GET /Users/{user}
//
// userGetHandler returns a user
func (j *Jellyfin) userGetHandler(w http.ResponseWriter, r *http.Request) {
	reqCtx := j.getRequestCtx(w, r)
	if reqCtx == nil {
		return
	}
	vars := mux.Vars(r)
	userID := vars["userid"]
	dbuser, err := j.repo.GetUserByID(r.Context(), userID)
	if err != nil {
		apierror(w, ErrUserIDNotFound, http.StatusNotFound)
		return
	}
	// Allow if requester is an administrator or user themselves
	if !reqCtx.User.Properties.Admin && reqCtx.User.ID != userID {
		apierror(w, "forbidden to access user", http.StatusForbidden)
		return
	}
	response := j.makeJFUser(r.Context(), dbuser)
	serveJSON(response, w)
}

// DELETE /Users/{user}
//
// userDeleteHandler deletes a user
func (j *Jellyfin) userDeleteHandler(w http.ResponseWriter, r *http.Request) {
	reqCtx := j.getRequestCtx(w, r)
	if reqCtx == nil {
		return
	}
	apierror(w, "account administration is available through the RCFin CLI", http.StatusForbidden)
}

// GET /Users/Public
//
// usersHandler returns list of public users
func (j *Jellyfin) usersPublicHandler(w http.ResponseWriter, r *http.Request) {
	users, err := j.repo.GetAllUsers(r.Context())
	if err != nil {
		apierror(w, "failed to get users", http.StatusInternalServerError)
		return
	}
	response := []JFUser{}
	for _, user := range users {
		if !user.Properties.IsHidden {
			response = append(response, j.makeJFUser(r.Context(), &user))
		}
	}
	serveJSON(response, w)
}

// POST /Users/{user}/Configuration
//
// usersConfigurationHandler updates user configuration
func (j *Jellyfin) usersConfigurationHandler(w http.ResponseWriter, r *http.Request) {
	reqCtx := j.getRequestCtx(w, r)
	if reqCtx == nil {
		return
	}
	vars := mux.Vars(r)
	userID := vars["userid"]
	// Only allow if requester is an administrator or the user themselves
	if !reqCtx.User.Properties.Admin && reqCtx.User.ID != userID {
		apierror(w, "forbidden to update user policy", http.StatusForbidden)
		return
	}
	dbuser, err := j.repo.GetUserByID(r.Context(), userID)
	if err != nil {
		apierror(w, ErrUserIDNotFound, http.StatusNotFound)
		return
	}
	// log.Printf("Looking up user by id: %s\n", userID)
	var req JFUserConfiguration
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror(w, "invalid request body", http.StatusBadRequest)
		return
	}
	// log.Printf("Updating user configuration %+v", req)
	parseJFUserConfiguration(req, &dbuser.Properties)
	if err = j.repo.UpsertUser(r.Context(), dbuser); err != nil {
		apierror(w, "failed to update user configuration", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /Users/{user}/Policy
//
// usersPolicyHandler updates user policy
func (j *Jellyfin) usersPolicyHandler(w http.ResponseWriter, r *http.Request) {
	reqCtx := j.getRequestCtx(w, r)
	if reqCtx == nil {
		return
	}
	apierror(w, "user policy changes are available through the RCFin CLI", http.StatusForbidden)
}

// POST /Users/Password
//
// usersPasswordHandler updates user password
func (j *Jellyfin) usersPasswordHandler(w http.ResponseWriter, r *http.Request) {
	reqCtx := j.getRequestCtx(w, r)
	if reqCtx == nil {
		return
	}
	queryparams := r.URL.Query()
	userID := queryparams.Get("userId")
	if userID == "" {
		userID = reqCtx.User.ID
	}
	// Password changes through the client are self-service only. Admin resets use the CLI.
	if reqCtx.User.ID != userID {
		apierror(w, "forbidden to update user password", http.StatusForbidden)
		return
	}
	dbuser, err := j.repo.GetUserByID(r.Context(), userID)
	if err != nil {
		apierror(w, ErrUserIDNotFound, http.StatusNotFound)
		return
	}
	var req JFUserPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.NewPw == "" {
		apierror(w, "new password is required", http.StatusBadRequest)
		return
	}
	currentPassword := req.CurrentPw
	if currentPassword == "" {
		currentPassword = req.CurrentPassword
	}
	if currentPassword == "" || validatePassword(dbuser.Password, currentPassword) != nil {
		apierror(w, "current password is invalid", http.StatusUnauthorized)
		return
	}
	hashedPassword, err := hashPassword(req.NewPw)
	if err != nil {
		apierror(w, "failed to hash password", http.StatusInternalServerError)
		return
	}
	dbuser.Password = hashedPassword
	if err = j.repo.UpsertUser(r.Context(), dbuser); err != nil {
		apierror(w, "failed to update user password", http.StatusInternalServerError)
		return
	}
	if err := revokeAccessTokens(r.Context(), j.repo, userID); err != nil {
		apierror(w, "password changed but failed to revoke existing sessions", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (j *Jellyfin) makeJFUser(ctx context.Context, user *model.User) JFUser {
	u := JFUser{
		Id:                        user.ID,
		Name:                      user.Username,
		ServerId:                  j.serverID,
		HasPassword:               true,
		HasConfiguredPassword:     true,
		HasConfiguredEasyPassword: false,
		EnableAutoLogin:           false,
		Configuration:             makeJFUserConfiguration(user),
		Policy:                    makeJFUserPolicy(user),
	}
	if !user.LastLogin.IsZero() {
		u.LastLoginDate = &user.LastLogin
	}
	if !user.LastUsed.IsZero() {
		u.LastActivityDate = &user.LastUsed
	}
	// Set imagetag if user has an image
	if _, err := j.repo.HasImage(ctx, user.ID, imageTypeProfile); err == nil {
		u.PrimaryImageTag = user.ID
	}
	return u
}

// makeJFUserConfiguration creates a JFUserConfiguration from the user properties
func makeJFUserConfiguration(user *model.User) JFUserConfiguration {
	return JFUserConfiguration{
		CastReceiverId:             "F007D354",
		GroupedFolders:             []string{},
		LatestItemsExcludes:        []string{},
		MyMediaExcludes:            user.Properties.MyMediaExcludes,
		OrderedViews:               user.Properties.OrderedViews,
		SubtitleMode:               "Default",
		PlayDefaultAudioTrack:      true,
		RememberAudioSelections:    true,
		RememberSubtitleSelections: true,
	}
}

// parseJFUserConfiguration parses the user configuration from the request and updates the user properties
func parseJFUserConfiguration(config JFUserConfiguration, props *model.UserProperties) {
	props.MyMediaExcludes = config.MyMediaExcludes
	props.OrderedViews = config.OrderedViews
}

// makeJFUserPolicy creates a JFUserPolicy from the user properties
func makeJFUserPolicy(user *model.User) JFUserPolicy {
	return JFUserPolicy{
		AccessSchedules:                  []string{},
		AllowedTags:                      user.Properties.AllowTags,
		BlockedChannels:                  []string{},
		BlockedMediaFolders:              []string{},
		BlockedTags:                      user.Properties.BlockTags,
		BlockUnratedItems:                user.Properties.BlockUnratedItems,
		EnabledChannels:                  []string{},
		EnabledDevices:                   []string{},
		EnabledFolders:                   user.Properties.EnabledFolders,
		EnableContentDeletionFromFolders: []string{},
		EnableContentDownloading:         user.Properties.EnableDownloads,
		EnableMediaPlayback:              true,
		EnableRemoteAccess:               true,
		EnableAllDevices:                 true,
		EnableAllFolders:                 user.Properties.EnableAllFolders,
		AuthenticationProviderID:         "DefaultAuthenticationProvider",
		PasswordResetProviderID:          "DefaultPasswordResetProvider",
		SyncPlayAccess:                   "CreateAndJoinGroups",
		IsAdministrator:                  user.Properties.Admin,
		IsDisabled:                       user.Properties.Disabled,
		IsHidden:                         user.Properties.IsHidden,
		MaxParentalRating:                user.Properties.MaxParentalRating,
	}
}

// parseJFUserPolicy parses the user policy from the request and updates the user properties
func parseJFUserPolicy(policy JFUserPolicy, props *model.UserProperties) {
	props.AllowTags = policy.AllowedTags
	props.BlockTags = policy.BlockedTags
	props.EnableAllFolders = policy.EnableAllFolders
	props.EnabledFolders = policy.EnabledFolders
	props.EnableDownloads = policy.EnableContentDownloading
	props.Admin = policy.IsAdministrator
	props.Disabled = policy.IsDisabled
	props.IsHidden = policy.IsHidden
	props.MaxParentalRating = policy.MaxParentalRating
	props.BlockUnratedItems = policy.BlockUnratedItems
}

// validatePassword validates a password against a hashed password
func validatePassword(hashedPassword, password string) error {
	return bcrypt.CompareHashAndPassword([]byte(hashedPassword), []byte(password))
}

// hashPassword hashes a password
func hashPassword(password string) (string, error) {
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hashedPassword), nil
}

func revokeAccessTokens(ctx context.Context, repo interface {
	GetAccessTokens(context.Context, string) ([]model.AccessToken, error)
	DeleteAccessToken(context.Context, string) error
}, userID string) error {
	tokens, err := repo.GetAccessTokens(ctx, userID)
	if err != nil {
		return err
	}
	for _, token := range tokens {
		if err := repo.DeleteAccessToken(ctx, token.Token); err != nil {
			return err
		}
	}
	return nil
}
