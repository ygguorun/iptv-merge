# 项目名称
PROJECT_NAME := iptv-merge

# 目标平台
PLATFORMS := linux_armv7 darwin_arm64

# Go 编译器设置
GO := go
# GO := garble

# 定义 LDFLAGS
LDFLAGS := "-s -w"

# 生成的文件存放目录
BUILD_DIR := build

# 生成文件名
define BUILD_OUTPUT
$(BUILD_DIR)/$(PROJECT_NAME)_$(if $(findstring armv7,$1),linux_armv7,$1)$(if $(findstring windows,$1),.exe)
endef

# 根据目标平台进行编译
all: $(PLATFORMS)

$(PLATFORMS):
	@mkdir -p $(BUILD_DIR)
	GOOS=$(word 1, $(subst _, ,$@)) \
	GOARCH=$(if $(findstring armv7,$@),arm,$(word 2, $(subst _, ,$@))) \
	CGO_ENABLED=0 $(GO) build -ldflags=$(LDFLAGS) -o $(call BUILD_OUTPUT,$@)

# 清理编译文件
clean:
	rm -rf $(BUILD_DIR)

.PHONY: all clean $(PLATFORMS)
