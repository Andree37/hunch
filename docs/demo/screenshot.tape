# The README screenshot: hello.yaml in the TUI after running one test case.
# Run docs/demo/screenshot.sh rather than this directly.
Output .demo/rec/screenshot.gif
Set Shell zsh
Set FontSize 32
Set Width 2800
Set Height 1300
Set Padding 40
Set Theme "Catppuccin Mocha"
Set WindowBar Colorful

Hide
Type "export PS1='$ ' && clear" Enter
Type "hunch tui examples/hello.yaml --model jev" Enter
Sleep 2s
Type "2"
Sleep 300ms
Enter
Sleep 500ms
Type "r"
Wait+Screen@60s /ended at/
Type "1"
Sleep 200ms
Show
Sleep 500ms
Screenshot .demo/rec/screenshot.png
Type "q"
