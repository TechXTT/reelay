# Release workflow

- **When to read:** Before re-running `.github/workflows/release.yml` for an existing tag, or when Jellyfin reports a checksum mismatch installing the Reelay plugin.

## Quick Reference

**Gotcha:** Every run attempt rebuilds the plugin zips with new MD5 checksums and uploads another `github-pages` artifact to the same run. `actions/deploy-pages` can deploy an older attempt's artifact, so the catalog's `checksum` no longer matches the release zip. Jellyfin then fails with "The checksums didn't match while installing "Reelay"".

**Action:** Before re-running, delete the release (keep the tag) and every `github-pages` artifact of the run (`gh api repos/TechXTT/reelay/actions/runs/<id>/artifacts`, then `gh api -X DELETE .../actions/artifacts/<artifact-id>`). Afterwards compare `md5sum` of each released zip with the `checksum` in `manifest.json` and `manifest-preview.json`.

## Evidence

On 2026-11-22 a v0.1.6 rerun after a transient deploy failure left the 10.11 catalog at `06bfa9b6…` while the released zip was `b6735037…`; the run listed two `github-pages` artifacts. Deleting both and re-running produced matching checksums.
