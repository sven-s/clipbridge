APP_NAME    = Clipbridge
BUILD_DIR   = build
BINARY      = $(BUILD_DIR)/clipbridge-mac
APP_BUNDLE  = $(BUILD_DIR)/$(APP_NAME).app
DMG         = $(BUILD_DIR)/$(APP_NAME).dmg
ICON_PNG    = assets/icon.png
ICON_ICNS   = assets/icon.icns

.PHONY: all build app dmg run clean deps icon

all: dmg

deps:
	go mod tidy

build: $(BINARY)

$(BINARY): $(shell find src -name '*.go')
	@mkdir -p $(BUILD_DIR)
	go build -ldflags="-s -w" -o $(BINARY) ./src/cmd/mac/

run: build
	$(BINARY)

icon: $(ICON_ICNS)

$(ICON_PNG): scripts/gen-icon.go
	go run scripts/gen-icon.go

$(ICON_ICNS): $(ICON_PNG)
	@rm -rf assets/AppIcon.iconset
	@mkdir -p assets/AppIcon.iconset
	sips -z 16 16     $(ICON_PNG) --out assets/AppIcon.iconset/icon_16x16.png       >/dev/null
	sips -z 32 32     $(ICON_PNG) --out assets/AppIcon.iconset/icon_16x16@2x.png    >/dev/null
	sips -z 32 32     $(ICON_PNG) --out assets/AppIcon.iconset/icon_32x32.png       >/dev/null
	sips -z 64 64     $(ICON_PNG) --out assets/AppIcon.iconset/icon_32x32@2x.png    >/dev/null
	sips -z 128 128   $(ICON_PNG) --out assets/AppIcon.iconset/icon_128x128.png     >/dev/null
	sips -z 256 256   $(ICON_PNG) --out assets/AppIcon.iconset/icon_128x128@2x.png  >/dev/null
	sips -z 256 256   $(ICON_PNG) --out assets/AppIcon.iconset/icon_256x256.png     >/dev/null
	sips -z 512 512   $(ICON_PNG) --out assets/AppIcon.iconset/icon_256x256@2x.png  >/dev/null
	sips -z 512 512   $(ICON_PNG) --out assets/AppIcon.iconset/icon_512x512.png     >/dev/null
	cp $(ICON_PNG) assets/AppIcon.iconset/icon_512x512@2x.png
	iconutil -c icns assets/AppIcon.iconset -o $(ICON_ICNS)
	@rm -rf assets/AppIcon.iconset

app: $(APP_BUNDLE)

$(APP_BUNDLE): $(BINARY) $(ICON_ICNS)
	@rm -rf $(APP_BUNDLE)
	@mkdir -p $(APP_BUNDLE)/Contents/MacOS
	@mkdir -p $(APP_BUNDLE)/Contents/Resources
	cp $(BINARY) $(APP_BUNDLE)/Contents/MacOS/$(APP_NAME)
	chmod +x $(APP_BUNDLE)/Contents/MacOS/$(APP_NAME)
	cp $(ICON_ICNS) $(APP_BUNDLE)/Contents/Resources/AppIcon.icns
	cp scripts/Info.plist $(APP_BUNDLE)/Contents/Info.plist
	@echo "→ $(APP_BUNDLE)"

dmg: $(DMG)

$(DMG): $(APP_BUNDLE)
	@rm -rf $(BUILD_DIR)/dmg-staging $(DMG)
	@mkdir -p $(BUILD_DIR)/dmg-staging
	cp -R $(APP_BUNDLE) $(BUILD_DIR)/dmg-staging/
	ln -s /Applications $(BUILD_DIR)/dmg-staging/Applications
	hdiutil create -volname "$(APP_NAME)" -srcfolder $(BUILD_DIR)/dmg-staging \
		-ov -format UDZO $(DMG) >/dev/null
	@rm -rf $(BUILD_DIR)/dmg-staging
	@echo "→ $(DMG)"

clean:
	rm -rf $(BUILD_DIR) assets/icon.png assets/icon.icns assets/AppIcon.iconset
