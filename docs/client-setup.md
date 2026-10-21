# Element X and Element Web setup

Rooms remain **unencrypted**: the homeserver stores their messages in
server-readable form. Account/device verification is separate from room
encryption. It establishes trust in a login; disabling room encryption does
not guarantee that clients will stop asking to verify devices.

Use the provisioned account, not public registration (which stays disabled).
For a fresh account, start with Element X and let it complete its supported
security setup. It may bootstrap its first device automatically; this is not
a guarantee for every app version. Set up recovery when offered and store the
recovery key privately. Never paste passwords or recovery keys into chat or logs.

Keep the phone signed in, then sign into the bundled Element Web website.
Verify the Web session using that trusted phone (compare the displayed
emoji/number sequence or use the offered verification method), or use the
recovery key. Verify only sessions you initiated and recognise.

If no trusted device or recovery key remains, the client's **Can't confirm?** /
reset-identity flow may be necessary. Read its data-loss warning first:
resetting an identity is not the same as verifying it, and may make encrypted
history inaccessible. A single device can recover using a saved recovery key;
a second device is not always required. Do not repeatedly reset identities to
silence a prompt.

Reopening the same signed-in sessions should not repeatedly require verification.
A new login, reinstallation or cleared client storage may need verification
again. Keep the trusted device and recovery key rather than signing out to
troubleshoot. If the prompt returns in an unchanged session, record the app
version and exact prompt and investigate before wiping state.

Bundled Web configuration does not configure the installed Element X app.
There is no verification-suppression module in this deployment. Actual first
login and subsequent-session behaviour must be checked with the installed
Element X version; automated bridge tests do not exercise the native app.
