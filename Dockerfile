FROM mcr.microsoft.com/windows/nanoserver:1809
COPY /win-registrator.exe /

ENTRYPOINT ["C:/win-registrator.exe"]