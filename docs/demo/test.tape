Output docs/demo/test.gif
Source docs/demo/settings.tape
Set Height 240

Hide
Type "export PS1='$ ' && clear" Enter
Show

Type "hunch test examples/hello.yaml --backend jev"
Sleep 500ms
Enter
Wait+Screen@60s /passed/
Sleep 4s
