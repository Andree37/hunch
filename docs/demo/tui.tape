Output docs/demo/hello-tui.gif
Source docs/demo/settings.tape
Set Width 1200
Set Height 560
Set TypingSpeed 120ms

Hide
Type "export PS1='$ ' && clear" Enter
Show

Type@40ms "hunch tui examples/hello.yaml --backend jev"
Sleep 400ms
Enter
Sleep 2s

# load the "meeting" test case and run it
Type "2"
Sleep 500ms
Enter
Sleep 1s
Type "r"
Wait+Screen@60s /[0-9]+ steps · \$/
Sleep 3s

# the next case, "newsletter", takes the other branch
Type "j"
Sleep 300ms
Enter
Sleep 800ms
Type "r"
Sleep 3.5s

# walk the steps
Type "1"
Type "v"
Sleep 1.5s
Type "j"
Sleep 2.5s
Type "q"
