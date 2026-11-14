# Account administration

RCFin accounts are stored in the configured SQLite database. Registration is not available through the API or clients. Create, disable, delete, promote, and grant library access to accounts from the host running RCFin. Run the command as the same OS account that can read the configuration, database, and related files:

```sh
./bin/rcfin --config /etc/rcfin/rcfin.yaml user list
```

Commands also accept `--config` after the subcommand. If omitted, RCFin reads `rcfin.yaml` from the current directory. CLI password entry is interactive and hidden; passwords are hashed before storage. They are never accepted as command-line arguments.

## First administrator

Create an administrator during initial setup. The first account is not created automatically:

```sh
./bin/rcfin --config /etc/rcfin/rcfin.yaml user add --username admin --admin
```

The command prompts twice for the password. An administrator can see every configured library and manage all accounts from the CLI. RCFin does not offer user registration or API-based account creation/deletion.

## User commands

| Command | Effect |
| --- | --- |
| `user add --username NAME` | Create a regular account with no library grants. |
| `user add --username NAME --admin` | Create an administrator with access to all libraries. |
| `user list` | List usernames, role, and enabled/disabled state. |
| `user password reset NAME` | Set a new password through hidden interactive prompts; existing sessions are revoked. |
| `user disable NAME` / `user enable NAME` | Disable or re-enable sign-in. Disabling revokes existing sessions. |
| `user delete NAME` | Delete the account and its saved user data. |
| `user grant-admin NAME` | Make an account an administrator with access to every library. |
| `user revoke-admin NAME` | Remove administrator privileges and clear all its library grants. |

For example:

```sh
./bin/rcfin --config /etc/rcfin/rcfin.yaml user add --username sam
./bin/rcfin --config /etc/rcfin/rcfin.yaml user password reset sam
./bin/rcfin --config /etc/rcfin/rcfin.yaml user disable sam
```

The final enabled administrator cannot be disabled, deleted, or demoted. Create or enable another administrator first.

## Library access

Every collection in `rcfin.yaml` needs an explicit, unique, stable `id`. These IDs are the access-control keys; library display names can change without affecting grants. View the configured IDs with:

```sh
./bin/rcfin --config /etc/rcfin/rcfin.yaml library list
```

Regular accounts begin with no access. Grant or revoke one library at a time, and inspect an account's grants with:

```sh
./bin/rcfin --config /etc/rcfin/rcfin.yaml user libraries sam
./bin/rcfin --config /etc/rcfin/rcfin.yaml user libraries sam grant movies
./bin/rcfin --config /etc/rcfin/rcfin.yaml user libraries sam revoke movies
```

Use the IDs printed by `library list`. A grant applies to the full library, including its items, metadata, artwork, and direct media stream. Administrators always bypass library grants and see all libraries. Grants for IDs absent from the active configuration do not expose anything; they can take effect again if that same stable ID is configured later.

The `v1.1.0` database migration revokes the prior implicit all-library access for existing non-admin accounts. Existing administrator status is preserved. After upgrading, explicitly grant each regular user the libraries they should see. Back up the SQLite database before upgrading, as with any database migration.

## Password changes in clients

Signed-in users may change their own password using a compatible Jellyfin client's password-change flow. The client must supply the current password. Administrative resets for another account are performed with `user password reset NAME` on the server host. There is no public password-reset or registration flow.

## Security and operating notes

- Treat the SQLite database as private account data. It contains password hashes, sessions, grants, and user state.
- Keep the configuration and database writable only by the RCFin service account and trusted operators.
- CLI commands use the database path in the selected config and apply pending database migrations before operating.
- Disabling an account blocks password and token authentication and revokes its stored sessions.
- Removing a library from the configuration leaves grants keyed to its ID in the database; access remains unavailable until the library is configured again.
