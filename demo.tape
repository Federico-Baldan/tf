# vhs demo of the tf wrapper — run from the repo root: vhs demo.tape
Output demo.gif

Set FontSize 17
Set Width 1100
Set Height 620
Set Padding 14
Set TypingSpeed 45ms

Hide
Type `export PATH="$PWD:$PATH" && cd demo && clear`
Enter
Show

# ---- apply: plan -> review tree -> approve -> live progress ----
Type "tf apply"
Enter
Wait+Screen /Plan: \+9/
Sleep 1.2s
Type "j"
Sleep 300ms
Type "j"
Sleep 300ms
Enter
Sleep 1.8s
Type "a"
Sleep 1.2s
Type "y"
Wait+Screen /Apply complete/
Sleep 1.5s

# ---- plan with changes: expand every diff ----
Type "tf plan -var flavor=chocolate"
Enter
Wait+Screen /Plan: ~4/
Sleep 1s
Type "e"
Sleep 2.8s
Type "q"
Sleep 1s

# ---- destroy: review gate with red warning ----
Type "tf destroy"
Enter
Wait+Screen /Destroy plan/
Sleep 1.2s
Type "a"
Sleep 1.5s
Type "y"
Wait+Screen /Destroy complete/
Sleep 1.5s
