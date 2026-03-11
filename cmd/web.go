package cmd

import _ "embed"

//go:embed web/dashboard.html
var dashboardHTML string

//go:embed web/kiosk.html
var kioskHTML string

//go:embed web/favicon.ico
var faviconICO []byte

//go:embed web/favicon-16x16.png
var favicon16 []byte

//go:embed web/favicon-32x32.png
var favicon32 []byte

//go:embed web/apple-touch-icon.png
var appleTouchIcon []byte

//go:embed web/android-chrome-192x192.png
var logoPNG []byte
