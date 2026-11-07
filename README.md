# PSG3 Desktop Save Collector

Open-source Windows utility for collecting **old** and **new** Pixel Survival Game 3 character save files (.es) for a transfer or editing request.

## Download

Download the latest Windows 64-bit EXE from [Releases](../../releases) after the build has completed.

## How to use

1. Enter an in-game name, the **required email address** for the eventual returned save, and the character slot.
2. Connect the Android device by USB file transfer and select the old character `.es` file.
3. Verify the independent old-save backup in **Downloads / PSG3 Collector Backups** before making any changes to the game account.
4. Create and save a new character in the same slot manually; then select its new `.es` file.
5. Click **Submit**. The app verifies and packages the old and new saves with submission details, including the return email, and uploads the ZIP over HTTPS.

**Important:** Account changes or resets are never performed by the collector. The collector does not edit saves, deliver email, or delete uploaded files. Submission does not guarantee account recovery. Only use it for game data you are authorised to modify.

## Privacy and architecture

- No submitted saves, real player email addresses, GitHub tokens, or secret keys are included in this public source repository.
- The EXE submits the selected saves and entered information to a separately hosted HTTPS receiver, configured during compilation.
- Submitted data is kept in a **separate, private repository**; this public repository contains source code only.
- Email addresses are required by the server for new submissions, but the collector cannot verify mailbox ownership.
- For information about requests and returns, contact the service operator. **Automatic save modification, email return and deletion are not part of this repository.**
- The client does not store account passwords or require a GitHub account from players.

## Build from source

Install [Go 1.23 or later](https://go.dev/dl/) and run:

```bash
go test ./desktop
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-H=windowsgui -s -w -X main.receiverURL=https://psg3-save-intake.onrender.com/submit" -o PSG3_Desktop_Save_Collector.exe ./desktop
```

The Windows release is built and published with GitHub Actions. The receiver URL is **not** a secret, and **no GitHub access token is embedded in the EXE**.

## License

MIT — see [LICENSE](LICENSE).
