# Model calls happen while hidden, so the timeline is the same on every
# recording and the zooms in tui.json stay in sync.
Output .demo/rec/tui.mp4
Source docs/demo/settings.tape

Hide
Type "export PS1='$ ' && clear" Enter
Show

Type "hunch tui examples/hello.yaml --backend jev"
Sleep 1.5s
Enter
Sleep 4s

# load the "meeting" test case
Type "2"
Sleep 500ms
Enter
Sleep 3.5s

# run it
Type "r"
Hide
Wait+Screen@60s /3 steps · \$/
Show
Sleep 11s

# the "newsletter" case goes the other way
Type "j"
Sleep 500ms
Enter
Sleep 500ms
Type "r"
Hide
Wait+Screen@60s /Outcome: archive/
Show
Sleep 7s

# path view: only what ran
Type "1"
Type "v"
Sleep 5s
Hide
Type "q"
