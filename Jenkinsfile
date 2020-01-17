def ws = "/data/jenkins/workspace/${JOB_NAME}-${BUILD_NUMBER}"

pipeline {
  agent {
    node {
      label 'fsi-build-tests'
      customWorkspace "${ws}/nri-kube-events"
    }
  }
  options {
    buildDiscarder(logRotator(numToKeepStr: '15'))
    ansiColor('xterm')
  }

  environment {
    GOPATH = "${ws}/go"
    PATH = "${GOPATH}/bin:${PATH}"
  }

  stages {
    stage('CI') {
      parallel {
        stage('Linting and Validation') {
          steps {
            sh 'make docker-lint'
          }
        }
        stage('Unit Tests') {
          steps {
            sh 'make docker-test'
          }
        }
      }
    }
  }
}
