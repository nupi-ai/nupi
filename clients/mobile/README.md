# Nupi Mobile Placeholder

The mobile client for the new voice pipeline has not been implemented yet.
This directory acts as a placeholder so we can track pending tasks in the
architecture plan:

- UI shell for pairing with a desktop daemon.
- Microphone capture and streaming to the audio ingress endpoint (`POST /audio/ingress` or gRPC `StreamAudioIn`).
- Playback of TTS audio returned by the daemon.
- Voice controls exposed through the same flows as the Tauri desktop app.

Mobile development is planned for a future phase (see Epic 7 – Unified gRPC Transport in the sprint backlog).
Once the mobile scaffolding is ready, this README should be replaced with the
actual project documentation.
