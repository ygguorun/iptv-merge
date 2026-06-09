# 项目名称
PROJECT_NAME := iptv-merge

# 目标平台
PLATFORMS := linux_amd64 linux_arm64 linux_armv7 darwin_amd64 darwin_arm64 windows_amd64 windows_arm64

# Go 编译器设置
GO := go
# GO := garble

# 版本信息，可在命令行覆盖，例如：make all VERSION=v1.0.0
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || printf dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || printf unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# 定义 LDFLAGS
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)

# 生成的文件存放目录
BUILD_DIR := build
HOST_GOEXE := $(shell $(GO) env GOEXE)

# 生成文件名
define GOOS
$(word 1, $(subst _, ,$1))
endef

define GOARCH
$(if $(findstring armv7,$1),arm,$(word 2, $(subst _, ,$1)))
endef

define GOARM
$(if $(findstring armv7,$1),7,)
endef

define EXT
$(if $(findstring windows,$1),.exe,)
endef

define BUILD_OUTPUT
$(BUILD_DIR)/$(PROJECT_NAME)_$1$(call EXT,$1)
endef

# 根据目标平台进行编译
all: $(PLATFORMS)

build:
	@mkdir -p $(BUILD_DIR)
	$(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BUILD_DIR)/$(PROJECT_NAME)$(HOST_GOEXE) .

$(PLATFORMS):
	@mkdir -p $(BUILD_DIR)
	GOOS=$(call GOOS,$@) \
	GOARCH=$(call GOARCH,$@) \
	GOARM=$(call GOARM,$@) \
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(call BUILD_OUTPUT,$@) .

# 清理编译文件
clean:
	rm -rf $(BUILD_DIR)

.PHONY: all build clean $(PLATFORMS)
