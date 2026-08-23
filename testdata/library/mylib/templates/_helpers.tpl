{{- define "mylib.fullname" -}}
{{ .Release.Name }}-{{ .Chart.Name }}
{{- end -}}
