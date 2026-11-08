# The README screenshot: hello.yaml in the TUI after running one test case.
# Run docs/demo/screenshot.sh rather than this directly.
Output .demo/rec/screenshot.gif
Set Shell zsh
Set FontSize 32
Set Width 2400
Set Height 1120
Set Padding 40
Set Theme "Catppuccin Mocha"
Set WindowBar Colorful

Hide
Type "export PS1='$ ' && clear" Enter
Type "hunch tui examples/hello.yaml --backend jev" Enter
Sleep 2s
Type "2"
Sleep 300ms
Enter
Sleep 500ms
Type "r"
Wait+Screen@60s /Outcome:/
Show
Sleep 500ms
Screenshot .demo/rec/screenshot.png
Type "q"
