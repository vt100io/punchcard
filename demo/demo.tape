# Run with: mise run demo (sets up the server this records)
Output demo/demo.gif

Set FontSize 16
Set Width 1400
Set Height 600
Set Padding 20
Set WindowBar Rings
Set Theme "TokyoNight"
Set TypingSpeed 60ms

# Start the webhooks, off camera.
Hide
Type "demo/scenario.sh >/dev/null 2>&1 & clear"
Enter
Show

Type "punchcard tui"
Sleep 400ms
Enter
Sleep 4s

# unit's push arrives and starts building: select it to follow its log.
Up
Sleep 10s

# Open the failed smed build from earlier.
Down
Sleep 700ms
Down
Sleep 3500ms

Type "q"
Sleep 500ms
