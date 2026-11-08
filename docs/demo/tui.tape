# The README's opening GIF: the flow, then two messages through it.
# Model calls happen while hidden, so the timeline is the same on every
# recording and the shots in tui.json stay in sync.
Output .demo/rec/tui.mp4
Source docs/demo/settings.tape

# 1. the flow itself
Hide
Type "export PS1='$ ' && clear" Enter
Type "clear && sed -n '/^nodes:/,$p' examples/hello.yaml" Enter
Sleep 500ms
Show
Sleep 17s

# 2. open it in the TUI
Hide
Type "clear" Enter
Sleep 300ms
Show
Type "hunch tui examples/hello.yaml --backend jev"
Sleep 1s
Enter
Sleep 5s

# 3. message 1: load the "meeting" test case
Type "2"
Sleep 500ms
Enter
Sleep 5s

# run it
Type "r"
Hide
Wait+Screen@60s /3 steps · \$/
Show
Sleep 10s

# select the second question
Type "1"
Sleep 300ms
Type "j"
Sleep 6s
Type "jj"
Sleep 5s

# 4. message 2: the "newsletter" test case
Type "2"
Sleep 300ms
Type "j"
Sleep 300ms
Enter
Sleep 5s
Type "r"
Hide
Wait+Screen@60s /Outcome: archive/
Type "1"
Type "kkkk"
Sleep 500ms
Show
Sleep 14s
Hide
Type "q"
