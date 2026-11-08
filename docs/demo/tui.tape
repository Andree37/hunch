Output docs/demo/tui.gif
Source docs/demo/settings.tape
Set Width 1400
Set Height 820
Set TypingSpeed 120ms

Hide
Type "export PS1='$ ' && clear" Enter
Show

Type@40ms "hunch tui"
Sleep 400ms
Enter
Sleep 2s

# pick network-triage
Type "jj"
Sleep 800ms
Enter
Sleep 1.5s

# load the "my computer is down (no internet)" test case
Type "2"
Sleep 300ms
Type "t"
Sleep 1s
Enter
Sleep 1.5s

# run it: Jev decides, writes stay dry
Type "r"
Wait+Screen@60s /[0-9]+ steps · \$/
Sleep 3s

# path view, then walk the steps
Type "1"
Type "v"
Sleep 2s
Type "j"
Sleep 1.5s
Type "j"
Sleep 1.5s
Type "j"
Sleep 1.5s
Type "j"
Sleep 2.5s
Type "q"
