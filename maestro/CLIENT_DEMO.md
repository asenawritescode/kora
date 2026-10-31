# Kora mobile client demo

This is a short, repeatable client walkthrough of the Kora mobile app using
the Tickets workspace. Maestro drives the app; the
voiceover is in `maestro/CLIENT_DEMO_VOICEOVER.md` and can be
generated separately and mixed with the screen recording.

## Demo path

1. Start at the signed-in Tickets Uat Workspace.
2. Browse to Ticket and review the existing records.
3. Create a sample ticket assigned to Administrator.
4. Show the saved ticket.

This highlights mobile navigation, record discovery, assignment, and creation.
The script generates a unique title on each run to keep the demo repeatable.

## Before recording

- Install/open the Kora Android app (`com.kora.mobile`) on the recording device.
- Connect and sign in to the Tickets workspace before recording. The flow
  expects the home screen heading `Tickets Uat Workspace` and creates a ticket.
- Use a seeded demo tenant or disposable tenant because each run adds a record.
- Connect and unlock the Android device, then leave exactly one authorized
  device attached (or pass its ID with `--device`).

Run the full screen capture and app walkthrough from the repository root:

```sh
scripts/record-kora-mobile-demo.sh
```

The wrapper starts Android screen recording, runs Maestro on the same device,
stops the recording, and pulls the MP4 to a timestamped folder under
`artifacts/mobile-client-demo/`. Maestro screenshots and its run output are
saved alongside it. It requires `adb` and `maestro` on PATH. Screen recording
is capped at three minutes; keep the demo flow within that limit.

You can select one of several attached devices with
`scripts/record-kora-mobile-demo.sh --device ID`.

The voiceover text is in `maestro/CLIENT_DEMO_VOICEOVER.md`. Audio generation
and mixing are not included in this command; add the generated narration in a
video editor and align the cues to the matching on-screen actions.

## Notes

- This is a mobile-app walkthrough; the checked-in repository contains the
  Maestro flows but not the mobile app source.
- The current app flow targets the signed-in Tickets workspace. Update its
  expected workspace name and selectors if the client demo uses another site.
