# Reelay activation checklist

Use this checklist to activate the implemented features on your Windows PC.
The Windows installation was activated on 2026-10-04 with the user's authorization.
Completed steps below reflect that installation; unchecked steps still need verification.

## Already verified

1. [x] Prowlarr is reachable on the Windows PC. Its endpoint
   `http://localhost:9696/1/api` passed capabilities, recent-results, and named-search
   checks on 2026-10-04. The recent listing returned 100 usable magnet video results.
2. [x] Go tests, vet, frontend typecheck/build, and Linux ARMv7 compilation passed.
3. [x] Browser fixtures covered previews, request controls, preferences, diagnostics,
   setup checks, backup downloads, and mobile layout.

4. [x] Reelay is running on schema 7 with Prowlarr enabled. Both NAS roots and
   their free-space checks pass; qBittorrent authentication/health passes.
5. [x] The updated Jellyfin plugin loaded and synchronized 19 real library items
   for both enabled users. A live TMDB preview returned a description, rating,
   and two videos.
6. [x] A post-activation backup restored into a separate database, passed integrity
   and schema validation, and retained matching row counts in every table.
7. [x] Both plugin test targets passed through the existing SDK fallback.
8. [x] ntfy accepted a connection confirmation and a real availability event
   from Reelay's outbox. Both messages were retrieved from the topic's cache.

New downloads/imports, live YouTube playback, and phone notification receipt
still need the checks below.

## Activate the Windows build

1. [x] **Preserve the current installation before migration.** Stop Reelay.
   Copy the file configured by `database.path`, any remaining adjacent `-wal` and
   `-shm` files, and `config.yaml` into a separate backup folder. Keep the current
   executable as well. Skip the database copy for a fresh installation.
   This PC's pre-migration files are in
   `data/backups/pre-activation-20261004-130023`.
   Do this before running any command that opens the database with the new binary:
   startup, `--check`, and `--backup` apply pending migrations. The new schema
   includes migrations 0006 and 0007; an older binary cannot open it afterward.
2. [x] **Build the UI, then the Windows executable** from the repository root:

   ```powershell
   .\make.ps1 web
   .\make.ps1 build
   ```

   The executable is `bin\reelay.exe`. The web build must come first because the
   executable embeds `web/dist`. These commands assume the development dependencies
   are already available; use the repository's installation targets if they are not.
3. [x] **Keep or create your configuration.** Existing installation: edit your
   current `config.yaml`. Fresh installation: copy `config.example.yaml` to
   `config.yaml` and configure the library roots, downloader, and quality profiles.
   Keep SQLite on local storage. Complete the Prowlarr and Discover sections below.
4. [x] **Validate and migrate** after preserving the old database:

   ```powershell
   .\bin\reelay.exe --config config.yaml --check
   ```

   This checks configuration and schema; it does not prove remote connectivity or
   that a Torznab credential environment variable is populated.
5. [x] **Start the new executable** in the same PowerShell session containing your
   credential environment variables:

   ```powershell
   .\bin\reelay.exe --config config.yaml
   ```

   If you use a service or scheduled task, update its executable path and provide
   the variables to that process. Variables set in your interactive shell are not
   automatically supplied to an already running service.
6. [x] **Verify setup checks.** The authenticated setup API passed. Open the dashboard
   at `http://127.0.0.1:7878/`, enter the Reelay bearer
   token if configured, and open **Settings → Setup checks**. Resolve missing
   paths, downloader failures, and insufficient free space. Indexer health here
   reports circuit-breaker state; use an actual search to check connectivity.

## Configure Prowlarr

1. [x] **Replace the existing direct Pirate Bay indexer entry** with this Torznab
   entry if you want to use Prowlarr for that source. Keep additional indexers only
   when intentional; pointing both entries at the same source repeats searches.

   ```yaml
   indexers:
     - name: prowlarr-tpb
       type: torznab
       enabled: true
       base_url: "http://127.0.0.1:9696/1/api"
       api_key_env: REELAY_PROWLARR_API_KEY
       user_agent: "Reelay"
       rate_limit_per_second: 0.1
       rate_limit_burst: 1
       request_timeout: 30s
       max_retries: 0
       failure_threshold: 5
       breaker_cooldown: 15m
       trackers:
         - "udp://tracker.opentrackr.org:1337/announce"
   ```

   Torznab calls are bounded; unsuccessful searches are retried by the engine's
   scheduled search/backoff flow. Torrent-file-only results without a usable
   magnet or infohash are skipped.
2. [x] **Supply your Prowlarr key without putting it in the documentation or shell
   history.** In the PowerShell session that will launch Reelay:

   ```powershell
   $reelayProwlarrKey = Read-Host 'Prowlarr API key' -AsSecureString
   $env:REELAY_PROWLARR_API_KEY = [System.Net.NetworkCredential]::new('', $reelayProwlarrKey).Password
   ```

   `api_key` accepts a literal instead of `api_key_env`, but do not configure both
   or commit secrets. The Prowlarr key does not replace your TMDB or Reelay token.
   This PC has an ignored `bin/start-local.ps1` launcher that reads the key from
   Prowlarr's existing local configuration and passes it only to the new process.
   Run it from PowerShell after stopping the current Reelay instance; it refuses
   to start a duplicate. No global environment variable was changed.
3. [x] **Use the correct host address.** The endpoint above works when Reelay and
   Prowlarr run on the same Windows PC. If Reelay runs on the NAS, use the Windows
   PC's reachable LAN address and ensure Prowlarr accepts connections from it.
   If the Prowlarr indexer is recreated, copy its new endpoint; its ID may change.
4. [ ] **Check connectivity through the new executable**, before starting a second
   Reelay instance:

   ```powershell
   .\bin\reelay.exe --config config.yaml --search Sintel
   ```

   This searches and prints decisions; it does not queue downloads. Results can be
   empty for a particular query. Investigate connection/API errors using Prowlarr's
   indexer Test and the Reelay logs.

## Configure Discover and Jellyfin

1. [x] **Configure a separate TMDB API key** via `metadata.tmdb_api_key` or the
   process variable `REELAY_METADATA_TMDB_API_KEY`. Set
   `recommendations.enabled: true`. Discover recommendations and previews require
   TMDB; a Prowlarr key cannot supply this metadata.
2. [x] **Install a compatible Reelay Jellyfin plugin.** Use the catalog for your
   server line described in the [README](../README.md#recommendation-plugin).
   For the changed plugin setup page from this working tree, build and install a
   matching plugin artifact; an older published artifact will retain its older UI.
   The plugin is separate from the Go executable.
   The previous installed plugin and its configuration are preserved under
   `F:\New folder\plugin-backups\activation-20261004-130359` on this PC.
3. [x] **Configure the plugin:** set the Reelay URL reachable from the Jellyfin
   host, Reelay bearer token, writable virtual library root, and enabled users.
   Test the connection, save, enable sync, and run **Sync Reelay Recommendations**
   from Jellyfin Scheduled Tasks.
4. [ ] **Create each user's Discover libraries.** Copy that user's movies and
   series paths from the plugin page. Add them as separate Movies and Shows
   libraries. In that user's Access settings, disable automatic access to all
   libraries, then grant their two Discover libraries and intended playable
   libraries. Scan the Discover libraries after the folders exist.
5. [ ] **Refresh Reelay Discover** for the intended user. Confirm sync and generation
   timestamps appear. Open Preview and check a description, audience rating, cast,
   runtime, trailer playback, and the external YouTube link. Some titles have no
   videos; verify the missing-video message rather than expecting every title to
   have a trailer.
6. [ ] **Set preferences if wanted.** Save language/genre filters and familiarity/
   diversity choices, then Refresh. Try a personal rating, edit it in history,
   and undo an unrated dismissal. TMDB ratings are out of 10; personal ratings are
   out of 5; match scores describe recommendation fit.

## Configure availability

1. [x] **Enable playable links.** After Jellyfin sync, obtain `server_id` from
   Reelay's authenticated `GET /api/v1/integrations/jellyfin/users` response. It is
   the plugin's configured server ID, not necessarily Jellyfin's own server ID.
   Map it to the Jellyfin base URL that your browser can open:

   ```yaml
   availability:
     jellyfin_servers:
       "REPLACE_WITH_SYNCED_PLUGIN_SERVER_ID": "http://YOUR_JELLYFIN_HOST:8096"
     webhook_url: ""
   ```

   Include any Jellyfin base path. Use the server root, without `/web`, a query,
   or a fragment. The link appears after an actual item is present in library sync.
2. [x] **Configure notifications.** ntfy is configured on this PC. Its topic URL
   is kept in the ignored `data/notification-url.txt`; open it in your browser or
   subscribe to the same topic in the ntfy phone app and allow notifications.
   Treat the random URL as a secret: an unprotected topic is accessible to anyone
   who knows its name. This topic is shared by the enabled household users.

   For another installation, set `availability.webhook_url` to a
   receiver that accepts Reelay's JSON `title_available` POST and returns a 2xx
   response. A receiver expecting a different message format needs a relay that
   transforms the payload. For direct ntfy delivery, set `webhook_format: ntfy`
   and use a random topic URL such as `https://ntfy.sh/YOUR_RANDOM_TOPIC`.
   This sends readable text without requester IDs; `json` is the default format.
   [ntfy's publishing documentation](https://docs.ntfy.sh/publish/) describes
   topic URLs and their access model. Subscribe in the browser or phone app;
   actual device receipt and background notifications still require your check.
3. [ ] **Optional: verify receiver deduplication and retry.** The event contains
   `request_id`, `server_id`, `user_id`, `media_type`, `tmdb_id`, `title`, and
   `jellyfin_item_id`. Deduplicate the stable `X-Reelay-Event-ID` header. Completed
   sync queues one event per active request, and delivery is at least once.
   An event already in flight may finish during withdrawal. Events can have been
   queued before you configured a receiver; enabling it can deliver that backlog.
4. [x] **Restart after configuration changes** and provide the same process
   credential variables again. Verify the webhook configuration in Setup checks.
   Imported files and Jellyfin-confirmed availability are separate states.
   A present series title does not prove every episode is available.

## Verify the complete live journey

1. [ ] Request one movie and confirm it persists under the correct user's Requests.
2. [ ] Request a series using Specific seasons. Check the selected seasons and
   episode counts; season 0 means specials. Future episodes wait for air date plus
   grace, and another user's broader monitoring continues.
3. [ ] Confirm qBittorrent uses the configured Reelay category and reported paths
   map to files Reelay can read. Watch progress through import and confirm the
   final file reaches the intended library root.
4. [ ] Scan the real Jellyfin library, complete plugin sync, and confirm **Available
   in Jellyfin** and **Open in Jellyfin** point to the playable item. If enabled,
   confirm the notification receiver gets the event.
5. [ ] On a test request needing recovery, use Attention needed and Diagnostics to
   inspect persisted search/import errors and retry timing. Select only an
   accepted candidate, or Retry an eligible failed/waiting subject.
6. [ ] Withdraw a test request and confirm its subscription ends. Shared downloads
   continue; use existing download controls separately if you intend to stop a
   shared transfer. Confirm another user's request remains intact.

## Backup and restore

1. [x] After activation, save a consistent snapshot using **Settings → Download
   database backup**, or use a new destination filename:

   ```powershell
   $reelayBackupPath = '.\backups\reelay-' + (Get-Date -Format 'yyyyMMdd-HHmmss') + '.db'
   .\bin\reelay.exe --config config.yaml --backup $reelayBackupPath
   ```

   The CLI opens and migrates the configured database before snapshotting. It does
   not overwrite an existing destination. Retain configuration and media separately.
   Local snapshots are `data/backups/post-activation-20261004.db` and
   `data/backups/notifications-enabled-20261004.db`.
2. [x] Test restore into an isolated destination. Stop Reelay before replacing
   its active database. Copy `config.yaml` to
   `config.restore.yaml` and change `database.path` in that copy to a new, absent
   local path. Restore and validate without replacing the original database:

   ```powershell
   .\bin\reelay.exe --config config.restore.yaml --restore .\backups\YOUR_BACKUP.db
   .\bin\reelay.exe --config config.restore.yaml --check
   ```

   Restore validates integrity and migration checksums. Inspect the restored data
   before choosing that database for normal service startup. Keep the original
   database and any remaining WAL/SHM companions together.

## Development checks and optional future work

1. [ ] Re-run `.\make.ps1 test` and `.\make.ps1 vet` after any code changes.
   Frontend changes require `.\make.ps1 web` before rebuilding the executable.
2. [ ] Complete race verification using a 64-bit C compiler or CI. The current
   compiler on PATH fails with `cc1.exe: sorry, unimplemented: 64-bit mode not
   compiled in`. This does not prevent the pure-Go release build.
3. [x] Build/test the plugin with `.\make.ps1 plugin-test` and the matching
   `plugin-10` or `plugin-12` build target. The launcher finds the matching SDK
   already installed outside PATH on this PC. Normal restore and both test
   targets passed on 2026-10-04 (six tests each). The installed plugin was built
   from this working tree for the existing Jellyfin server line.
   Use the SDK constraints in `global.json` and pinned plugin targets; do not
   change them just to match the default SDK on PATH.
4. [ ] Decide whether household members need direct dashboard access. Separate
   end-user authentication, request permissions, and quotas are future work.
   The current user selector is an operator filter under one bearer token.

See the [delivery status](product-plan.md#delivery-status) and
[architecture](architecture.md) for implementation details and verification limits.
