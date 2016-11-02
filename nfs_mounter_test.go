package nfsdriver_test

import (
	"errors"

	"code.cloudfoundry.org/goshims/execshim/exec_fake"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagertest"
	"code.cloudfoundry.org/nfsdriver"
	"context"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("NfsMounter", func() {

	var (
		logger lager.Logger
		err    error

		fakeExec *exec_fake.FakeExec

		subject nfsdriver.Mounter

		testContext context.Context
	)

	BeforeEach(func() {
		logger = lagertest.NewTestLogger("nfs-mounter")

		testContext = context.TODO()

		fakeExec = &exec_fake.FakeExec{}

		subject = nfsdriver.NewNfsMounter(fakeExec)
	})

	Context("#Mount", func() {
		var (
			output []byte

			fakeCmd *exec_fake.FakeCmd
		)

		Context("when mount succeeds", func() {
			BeforeEach(func() {
				var ptr uintptr

				fakeCmd = &exec_fake.FakeCmd{}
				fakeExec.CommandContextReturns(fakeCmd)

				output, err = subject.Mount(testContext, "source", "target", "my-fs", ptr, "my-mount-options")
			})

			It("should return without error", func() {
				Expect(err).NotTo(HaveOccurred())
			})

			It("should use the passed in variables", func() {
				_, cmd, args := fakeExec.CommandContextArgsForCall(0)
				Expect(cmd).To(Equal("mount"))
				Expect(args[0]).To(Equal("-t"))
				Expect(args[1]).To(Equal("my-fs"))
				Expect(args[2]).To(Equal("-o"))
				Expect(args[3]).To(Equal("my-mount-options"))
				Expect(args[4]).To(Equal("source"))
				Expect(args[5]).To(Equal("target"))
			})
		})

		Context("when mount errors", func() {
			BeforeEach(func() {
				var ptr uintptr

				fakeCmd = &exec_fake.FakeCmd{}
				fakeExec.CommandContextReturns(fakeCmd)

				fakeCmd.CombinedOutputReturns(nil, errors.New("badness"))

				output, err = subject.Mount(testContext, "source", "target", "my-fs", ptr, "my-mount-options")
			})

			It("should return without error", func() {
				Expect(err).To(HaveOccurred())
			})
		})

		Context("when mount is cancelled", func() {
			// TODO: when we pick up the lager.Context
		})
	})

	Context("#Unmount", func() {
		var (
			fakeCmd *exec_fake.FakeCmd
		)

		Context("when mount succeeds", func() {

			BeforeEach(func() {
				fakeCmd = &exec_fake.FakeCmd{}
				fakeExec.CommandContextReturns(fakeCmd)

				err = subject.Unmount(testContext, "target", 0)
			})

			It("should return without error", func() {
				Expect(err).NotTo(HaveOccurred())
			})

			It("should use the passed in variables", func() {
				_, cmd, args := fakeExec.CommandContextArgsForCall(0)
				Expect(cmd).To(Equal("umount"))
				Expect(args[0]).To(Equal("target"))
			})
		})

		Context("when unmount fails", func() {
			BeforeEach(func() {
				fakeCmd = &exec_fake.FakeCmd{}
				fakeExec.CommandContextReturns(fakeCmd)

				fakeCmd.RunReturns(errors.New("badness"))

				err = subject.Unmount(testContext, "target", 0)
			})

			It("should return an error", func() {
				Expect(err).To(HaveOccurred())
			})
		})
	})
})
