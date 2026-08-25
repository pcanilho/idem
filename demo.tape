# A 20-second demo of idem, for the README.
#
# Needs https://github.com/charmbracelet/vhs:
#     vhs demo.tape
#
# Deliberately generated from the real binary and the real example charts, so
# it cannot drift into showing output idem does not produce — which is the same
# reason readme_test.go exists.

Output docs/demo.gif

Set Shell zsh
Set FontSize 16
Set Width 1100
Set Height 800
Set Padding 20

Type "go build -o /tmp/idem ."
Enter
Sleep 3s

Type "clear"
Enter

Type "# a chart that cannot converge under ArgoCD"
Enter
Sleep 1s

Type "/tmp/idem ./examples/churning-chart"
Enter
Sleep 8s

Type "clear"
Enter

Type "# and one that is fine"
Enter
Sleep 1s

Type "/tmp/idem ./examples/stable-chart"
Enter
Sleep 4s
