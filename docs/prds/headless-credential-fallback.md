# Rescrobble: local encrypted credential fallback for headless Linux

## Problem and motivation

Rescrobble stores a Last.fm session credential exclusively in the
host's native OS credential service — Windows Credential Manager,
macOS Keychain, or Linux Secret Service — and today has no fallback of
any kind. On Windows and macOS that native store is always present. On
Linux it depends on a Secret Service provider (`gnome-keyring`,
`oo7-daemon`, KDE Wallet, etc.) being installed, running on a D-Bus
session bus, and unlocked.

Desktop Linux sessions get this for free. Headless Linux hosts —
servers, SSH-only boxes, containers, machines managed by
configuration tools like Ansible — do not. To use `login` or any
authenticated command there today, an operator must: install a
Secret Service daemon, start a D-Bus session for it, and either
interactively unlock its default collection or wire up a PAM module
(`pam_oo7.so` or equivalent) against their login stack just to hold
one credential. That's a disproportionate amount of system-level setup
for a single-user CLI tool, and it's setup most operators won't
discover how to do correctly without external guidance — `doctor`
today can only tell them the store is unavailable, not walk them
through fixing it.

This PRD adds a second tier: when no native OS credential service is
usable, rescrobble transparently falls back to a locally encrypted
file, so `login` and authenticated commands work out of the box on a
bare headless Linux host with zero additional system configuration.
The native OS store remains the preferred, tried-first mechanism
everywhere it's available — this fallback only activates in its
absence, and only on Linux; Windows and macOS keep today's
OS-store-only behavior since a native store is always present there.

## What "fallback" means here, concretely

The fallback store must never write the session credential in
plaintext. It encrypts the credential at rest (a modern AEAD cipher,
e.g. AES-256-GCM) using a key derived from machine-specific material
(e.g. `/etc/machine-id` combined with the owning user's identity, via
a standard KDF such as HKDF-SHA256) rather than a user-supplied
passphrase. Concretely, this means:

- No operator setup and no interactive prompt: the key is derivable
  entirely from local machine/user state, so `login` succeeds
  non-interactively the first time it runs on a bare host.
- The encrypted file is not portable: copying it to a different
  machine (different `/etc/machine-id`) must not be sufficient to
  decrypt it there.
- This is a strictly weaker guarantee than a native OS keyring: a
  local attacker with access to the same user account (or root) on
  the same host can reconstruct the key and decrypt the file, because
  the key material lives on that same host. This is an explicit,
  documented trade-off versus "no protection at all" (plaintext), not
  parity with a real OS credential service. The README and any
  relevant help/error text must state this trade-off plainly, not
  imply the fallback is as strong as the native store.

Follow the existing on-disk conventions already established by
`internal/journal` and `internal/config` (`os.UserConfigDir()`-rooted,
per-profile files, one file per profile, file locking around
read/modify/write) rather than inventing a new storage layout.

## Capabilities

**Encrypted file-backed credential store.** A new `credentials.Store`
implementation that encrypts/decrypts a profile's session credential
using a machine-derived key, persists it under the user's config
directory with owner-only permissions, and supports the same
`Save`/`Load`/`Delete`/`Has` operations as the existing OS-backed
store, including the existing `ErrCredentialNotFound` semantics for an
absent credential.

**Automatic, transparent fallback selection.** On Linux, when the
native OS store is unavailable (Secret Service absent, unreachable, or
otherwise unusable — the same condition that produces today's
`ErrSecureStoreUnavailable`), rescrobble automatically uses the
encrypted file store instead, with no flag or environment variable
required to opt in. When the native store *is* available, behavior is
byte-for-byte unchanged from today: it's used, and the fallback is
never consulted. Windows and macOS are unaffected by this capability —
they continue to require the native store exclusively, as today.

**Diagnostics and documentation reflect the two-tier model honestly.**
`doctor` reports which backend is actually protecting the active
profile's credential (native OS store vs. local encrypted fallback),
not just pass/fail. The README's current "no fallback exists" language
is replaced with an accurate description of both tiers and the
security trade-off of the fallback, including guidance that installing
a native Secret Service provider upgrades an existing fallback-backed
profile back to the stronger guarantee (see open question below on
whether that upgrade is automatic or manual).

## Out of scope

- A user-supplied passphrase-based fallback. This PRD is specifically
  the zero-configuration, machine-key-derived fallback; a
  passphrase-protected option (stronger, but requires the operator to
  supply and manage a secret) is not part of this work.
- Any fallback for Windows or macOS. Those platforms always have a
  native store; this PRD does not add a second tier there.
- Automatic migration of a credential between backends when
  availability changes mid-use (e.g., a Secret Service daemon becomes
  available after a profile was already using the fallback). Whether
  that transition is automatic or requires an explicit re-`login` is
  the open question below, but building an automatic migration path
  is not assumed to be in scope.
- Any change to how credentials are transmitted to or from Last.fm, or
  to the config/journal storage this PRD's file conventions are
  borrowed from.

## Success criteria

- On a Linux host with no Secret Service provider installed or
  running at all, `login` and subsequent authenticated commands
  succeed without the operator installing, configuring, or unlocking
  any OS-level credential service.
- The credential at rest is never plaintext under this fallback, and
  the encrypted file is confirmed non-portable across machine
  identities (decryption fails on a different host's machine-derived
  key).
- On a host where the native OS store is available and working,
  behavior, error messages, and `doctor` output for that path are
  unchanged from today — the fallback is invisible when it isn't
  needed.
- `doctor` output tells the operator, in plain language, which tier is
  protecting their active profile's credential and what that implies.

## Open questions

- **Upgrade/downgrade path**: if a profile's credential was created
  under the fallback and a native Secret Service later becomes
  available (or vice versa), should rescrobble migrate it
  automatically on next use, or require an explicit re-`login`? This
  PRD doesn't assume an answer; whichever is simpler and safer to
  implement first is fine, but it needs an explicit decision before
  implementation, not an implicit one.
- **Machine-identity loss**: reinstalling the OS, regenerating
  `/etc/machine-id`, or moving to a new machine invalidates the
  derived key by design. This should be treated as equivalent to "no
  credential saved" (re-`login` required), and documented as such, not
  treated as a bug to work around.
