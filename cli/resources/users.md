## User management

An AIS cluster can be deployed with [AuthN](/authn/README.md) - AIS authorization server. The CLI provides an easy way to manage users, and to grant and revoke access permissions.

If the AIS cluster does not have guest access enabled, every user that needs access to the cluster data must be registered. Guest access allows unregistered users to use the AIS cluster in read-only mode.

## Command List

All commands (except logout) requires AuthN URL that can be either passed in command line `AUTHN_URL=http://AUTHNSRV ais auth add ...` or export environment variable `export AUTHN_URL=http://AUTNSRV`. Where `AUTHNSRV` is hostname:port of AuthN server.

Adding and removing a user requires superuser permissions. Superuser login and password can be provided in command line:

`AUTHN_SU_NAME=admin AUTHN_SU_PASS=admin ais auth add ...`

or you can set environment variables globally with:

```shell
$ export AUTHN_SU_NAME=admin
$ export AUTHN_SU_PASS=admin
$ ais auth add ...
```

or just run:

`ais auth add..`

In the last case the CLI detects that there is not enough information and prompts for missing data in interactive mode. E.g, you can keep superuser name in environment variable and `ais` will prompt for superuser\'s password:

```shell
$ export AUTHN_SU_NAME=admin
$ ais auth add ...
Superuser login:
```

### Register a new user

`ais auth add USER_NAME USER_PASS`

Registers the user if a user with the same name does not exist yet and grants full access permissions to cluster data. For security reasons user\'s password can be omitted(and user\'s name as well). In this case the CLI prompts for every missing argument in interactive mode.

**Examples:**

Everything is set in command line:

`AUTHN_URL=http://AUTHNSRV AUTHN_SU_NAME=admin AUTHN_SU_PASS=admin ais auth add username password`

AuthN URL and superuser\'s name are already exported. Short command:

```shell
$ ais auth add username
Superuser password: admin
User password: password
```

### Unregister an existing user

`ais auth rm USER_NAME`

Removes existing user and revokes user's tokens if any one was issue for the user.

### Log in to AIS cluster

`ais auth login USER_NAME USER_PASS`

Issues a token for a user. After successful login, user\'s token is saved to `~/.ais/token` and next CLI runs automatically load and use the token in every request to AIS cluster. The saved token can be used by other application, like `curl`. Please see [AuthN documentation](/authn/README.md) to read how to use AuthN API directly.

### Log out

`ais auth logout`

Erases user\'s token from local machine, so the CLI switches to read-only mode(if guest access is enabled for AIS cluster). But other application still can use the issued token. To forbid using the token from any application, the token must be revoked in addition to logging out.
