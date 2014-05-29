VERSION := 3.14.4

linux: linux-$(VERSION)/linux
	cp linux-$(VERSION)/linux .

linux-$(VERSION)/linux: linux-$(VERSION)/.config
	cd linux-$(VERSION) && ARCH=um make -j8 && strip linux

linux-$(VERSION)/.config: linux-$(VERSION)/Makefile
	cp linux-config linux-$(VERSION)/.config
	touch linux-$(VERSION)/.config

linux-$(VERSION)/Makefile:
	curl -L https://www.kernel.org/pub/linux/kernel/v3.x/linux-$(VERSION).tar.xz | xz -d | tar x
	touch linux-$(VERSION)/Makefile

.PHONY: clean
clean:
	rm -rf linux linux-$(VERSION)
