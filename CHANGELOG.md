# Changelog

## [Sin publicar]

### Decisiones

- Motor, simulador y herramienta de depuración en Go; verificador en Python sin dependencias, sin código compartido con el motor.
- Cola y registro de eventos en SQLite (WAL, `synchronous=FULL`), elegido tras medir cero pérdidas en 30 `kill -9`.
- Toda corrida ocurre en un espacio de nombres de red sin privilegios con solo `lo`: el tráfico no puede salir de la máquina.
- Haraka 3.3.5 como MTA externo para la validación de caja negra.
- Licencia MIT.
