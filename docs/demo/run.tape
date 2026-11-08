Output docs/demo/run.gif
Source docs/demo/settings.tape
Set Height 420

Hide
Type "export PS1='$ ' && clear" Enter
Show

Type "hunch run examples/inbox.yaml --backend jev --set sender=boss --set channel=email --set vip=yes \" Enter
Type "  --set message='Can we sync Thursday? Need your numbers before the board meeting.'"
Sleep 500ms
Enter
Wait+Screen@60s /done ·/
Sleep 4s
