# Windows Registrator

Service registry bridge for Windows Docker.

##  consul demo

running Registrator looks like this:

docker run -d -u ContainerAdministrator -v \\\\.\pipe\docker_engine:\\\\.\pipe\docker_engine  win-registrator -internal=true -resync=30 -cleanup  consul://192.168.xx.xx:8500


## License

MIT

<img src="https://ga-beacon.appspot.com/UA-58928488-2/registrator/readme?pixel" />
