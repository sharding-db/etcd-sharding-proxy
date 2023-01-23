package server

import "golang.org/x/sync/errgroup"

type GroupRunnerFactory interface {
	GetGroupRunner() GroupRunner
}

type GroupRunnerFactoryImpl struct {
}

func NewDefaultGroupRunnerFactory() GroupRunnerFactory {
	return &GroupRunnerFactoryImpl{}
}

func (d *GroupRunnerFactoryImpl) GetGroupRunner() GroupRunner {
	return &GroupRunnerImpl{}
}

type GroupRunner interface {
	Add(func() error)
	Do() error
}

type GroupRunnerImpl struct {
	funcs []func() error
}

func (d *GroupRunnerImpl) Add(f func() error) {
	d.funcs = append(d.funcs, f)
}

func (d *GroupRunnerImpl) Do() error {
	eg := new(errgroup.Group)
	for _, f := range d.funcs {
		eg.Go(f)
	}
	return eg.Wait()
}
