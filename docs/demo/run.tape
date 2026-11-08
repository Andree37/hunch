Output docs/demo/hello-run.gif
Source docs/demo/settings.tape
Set Height 240

Hide
Type "export PS1='$ ' && clear" Enter
Show

Type `hunch run examples/hello.yaml --backend jev --set message="Can we sync Thursday?"`
Sleep 500ms
Enter
Wait+Screen@60s /done ·/
Sleep 4s
