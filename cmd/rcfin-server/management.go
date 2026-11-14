package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"github.com/adhikariprivate/rcfin-server/internal/database"
	"github.com/adhikariprivate/rcfin-server/internal/database/model"
	"github.com/adhikariprivate/rcfin-server/internal/idhash"
)

func managementCommand(args []string) (commandArgs []string, configPath string, ok bool) {
	configPath = "rcfin.yaml"
	remaining := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--config" {
			if i+1 >= len(args) {
				return nil, configPath, false
			}
			configPath = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(args[i], "--config=") {
			configPath = strings.TrimPrefix(args[i], "--config=")
			continue
		}
		remaining = append(remaining, args[i])
	}
	if len(remaining) == 0 || (remaining[0] != "user" && remaining[0] != "library") {
		return nil, configPath, false
	}
	return remaining, configPath, true
}

func runManagementCommand(configPath string, args []string) error {
	config, err := readConfig(configPath)
	if err != nil {
		return fmt.Errorf("read config %q: %w", configPath, err)
	}
	if err := validateCollections(config.Collections); err != nil {
		return fmt.Errorf("invalid collections: %w", err)
	}
	repo, err := openConfiguredDatabase(config)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer repo.Close()

	ctx := context.Background()
	switch args[0] {
	case "library":
		if len(args) != 2 || args[1] != "list" {
			return errors.New("usage: rcfin library list")
		}
		for _, library := range config.Collections {
			fmt.Printf("%-20s %-8s %s\n", library.ID, library.Type, library.Name)
		}
		return nil
	case "user":
		return runUserCommand(ctx, repo, config.Collections, args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runUserCommand(ctx context.Context, repo database.Repository, libraries []collectionConfig, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: rcfin user <add|list|delete|disable|enable|grant-admin|revoke-admin|password|libraries>")
	}
	switch args[0] {
	case "add":
		flags := pflag.NewFlagSet("user add", pflag.ContinueOnError)
		flags.SetOutput(io.Discard)
		usernameFlag := flags.String("username", "", "Account username")
		admin := flags.Bool("admin", false, "Create as administrator")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		username := strings.ToLower(strings.TrimSpace(*usernameFlag))
		if flags.NArg() != 0 || username == "" {
			return errors.New("usage: rcfin user add --username NAME [--admin]")
		}
		if _, err := repo.GetUser(ctx, username); err == nil {
			return fmt.Errorf("user %q already exists", username)
		} else if !errors.Is(err, model.ErrNotFound) {
			return fmt.Errorf("look up user: %w", err)
		}
		password, err := promptPassword("New password: ")
		if err != nil {
			return err
		}
		confirmation, err := promptPassword("Confirm password: ")
		if err != nil {
			return err
		}
		if password == "" || password != confirmation {
			return errors.New("passwords must be non-empty and match")
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("hash password: %w", err)
		}
		props := model.UserPropertiesDefault()
		props.Admin = *admin
		props.EnableAllFolders = *admin
		props.EnabledFolders = []string{}
		user := &model.User{ID: idhash.NewRandomID(), Username: username, Password: string(hash), CreatedAt: time.Now().UTC(), Properties: props}
		if err := repo.UpsertUser(ctx, user); err != nil {
			return fmt.Errorf("create user: %w", err)
		}
		fmt.Printf("Created user %q (admin=%t)\n", user.Username, props.Admin)
		return nil
	case "list":
		if len(args) != 1 {
			return errors.New("usage: rcfin user list")
		}
		users, err := repo.GetAllUsers(ctx)
		if err != nil {
			return fmt.Errorf("list users: %w", err)
		}
		for _, user := range users {
			state, role := "enabled", "user"
			if user.Properties.Disabled {
				state = "disabled"
			}
			if user.Properties.Admin {
				role = "admin"
			}
			fmt.Printf("%-24s %-8s %s\n", user.Username, role, state)
		}
		return nil
	case "password":
		if len(args) != 3 || args[1] != "reset" {
			return errors.New("usage: rcfin user password reset USERNAME")
		}
		user, err := getUserByName(ctx, repo, args[2])
		if err != nil {
			return err
		}
		if err := resetUserPassword(ctx, repo, user); err != nil {
			return err
		}
		fmt.Printf("Reset password for %q\n", user.Username)
		return nil
	case "disable", "enable", "delete", "grant-admin", "revoke-admin":
		if len(args) != 2 {
			return fmt.Errorf("usage: rcfin user %s USERNAME", args[0])
		}
		user, err := getUserByName(ctx, repo, args[1])
		if err != nil {
			return err
		}
		switch args[0] {
		case "disable":
			if user.Properties.Admin && !user.Properties.Disabled && enabledAdminCount(ctx, repo, user.ID) == 0 {
				return errors.New("cannot disable the last enabled administrator")
			}
			user.Properties.Disabled = true
			if err := repo.UpsertUser(ctx, user); err != nil {
				return fmt.Errorf("disable user: %w", err)
			}
			if err := revokeUserTokens(ctx, repo, user.ID); err != nil {
				return err
			}
		case "enable":
			user.Properties.Disabled = false
			if err := repo.UpsertUser(ctx, user); err != nil {
				return fmt.Errorf("enable user: %w", err)
			}
		case "delete":
			if user.Properties.Admin && enabledAdminCount(ctx, repo, user.ID) == 0 {
				return errors.New("cannot delete the last enabled administrator")
			}
			if err := repo.DeleteUser(ctx, user.ID); err != nil {
				return fmt.Errorf("delete user: %w", err)
			}
		case "grant-admin":
			user.Properties.Admin = true
			user.Properties.EnableAllFolders = true
			user.Properties.EnableDownloads = true
			if err := repo.UpsertUser(ctx, user); err != nil {
				return fmt.Errorf("promote user: %w", err)
			}
		case "revoke-admin":
			if !user.Properties.Admin {
				return fmt.Errorf("user %q is not an administrator", user.Username)
			}
			if user.Properties.Admin && enabledAdminCount(ctx, repo, user.ID) == 0 {
				return errors.New("cannot revoke admin from the last enabled administrator")
			}
			user.Properties.Admin = false
			user.Properties.EnableAllFolders = false
			user.Properties.EnabledFolders = []string{}
			if err := repo.UpsertUser(ctx, user); err != nil {
				return fmt.Errorf("revoke admin: %w", err)
			}
		}
		fmt.Printf("Updated user %q\n", user.Username)
		return nil
	case "libraries":
		if len(args) < 2 {
			return errors.New("usage: rcfin user libraries USERNAME [grant|revoke LIBRARY_ID]")
		}
		user, err := getUserByName(ctx, repo, args[1])
		if err != nil {
			return err
		}
		if len(args) == 2 {
			if user.Properties.Admin {
				fmt.Printf("%s: all libraries (admin)\n", user.Username)
				return nil
			}
			for _, library := range libraries {
				access := "denied"
				if user.Properties.EnableAllFolders || contains(user.Properties.EnabledFolders, library.ID) {
					access = "allowed"
				}
				fmt.Printf("%-20s %s\n", library.ID, access)
			}
			return nil
		}
		if len(args) != 4 || (args[2] != "grant" && args[2] != "revoke") {
			return errors.New("usage: rcfin user libraries USERNAME [grant|revoke LIBRARY_ID]")
		}
		if user.Properties.Admin {
			return errors.New("admins already have access to all libraries; change role before editing grants")
		}
		libraryID := args[3]
		if !configuredLibrary(libraries, libraryID) {
			return fmt.Errorf("unknown library ID %q; run 'rcfin library list'", libraryID)
		}
		user.Properties.EnableAllFolders = false
		user.Properties.EnabledFolders = removeString(user.Properties.EnabledFolders, libraryID)
		if args[2] == "grant" {
			user.Properties.EnabledFolders = append(user.Properties.EnabledFolders, libraryID)
		}
		if err := repo.UpsertUser(ctx, user); err != nil {
			return fmt.Errorf("update library grants: %w", err)
		}
		verb := "revoked"
		if args[2] == "grant" {
			verb = "granted"
		}
		fmt.Printf("Library %q %s for %q\n", libraryID, verb, user.Username)
		return nil
	default:
		return fmt.Errorf("unknown user command %q", args[0])
	}
}

func promptPassword(label string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("password entry requires an interactive terminal")
	}
	fmt.Fprint(os.Stderr, label)
	value, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return string(value), err
}

func getUserByName(ctx context.Context, repo database.Repository, username string) (*model.User, error) {
	user, err := repo.GetUser(ctx, strings.ToLower(strings.TrimSpace(username)))
	if errors.Is(err, model.ErrNotFound) {
		return nil, fmt.Errorf("user %q not found", username)
	}
	if err != nil {
		return nil, fmt.Errorf("look up user %q: %w", username, err)
	}
	return user, nil
}

func resetUserPassword(ctx context.Context, repo database.Repository, user *model.User) error {
	password, err := promptPassword("New password: ")
	if err != nil {
		return err
	}
	confirmation, err := promptPassword("Confirm password: ")
	if err != nil {
		return err
	}
	if password == "" || password != confirmation {
		return errors.New("passwords must be non-empty and match")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	user.Password = string(hash)
	if err := repo.UpsertUser(ctx, user); err != nil {
		return fmt.Errorf("save password: %w", err)
	}
	return revokeUserTokens(ctx, repo, user.ID)
}

func revokeUserTokens(ctx context.Context, repo database.Repository, userID string) error {
	tokens, err := repo.GetAccessTokens(ctx, userID)
	if err != nil {
		return fmt.Errorf("list user sessions: %w", err)
	}
	for _, token := range tokens {
		if err := repo.DeleteAccessToken(ctx, token.Token); err != nil {
			return fmt.Errorf("revoke user session: %w", err)
		}
	}
	return nil
}

func enabledAdminCount(ctx context.Context, repo database.Repository, excluding string) int {
	users, err := repo.GetAllUsers(ctx)
	if err != nil {
		return 0
	}
	count := 0
	for _, user := range users {
		if user.ID != excluding && user.Properties.Admin && !user.Properties.Disabled {
			count++
		}
	}
	return count
}

func configuredLibrary(libraries []collectionConfig, id string) bool {
	for _, library := range libraries {
		if library.ID == id {
			return true
		}
	}
	return false
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func removeString(values []string, value string) []string {
	result := make([]string, 0, len(values))
	for _, candidate := range values {
		if candidate != value {
			result = append(result, candidate)
		}
	}
	return result
}
