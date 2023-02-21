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
	return &errgroup.Group{}
}

type GroupRunner interface {
	Go(func() error)
	Wait() error
}
